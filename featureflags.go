package posthog

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const LONG_SCALE = 0xfffffffffffffff

type FeatureFlagsPoller struct {
	ticker                       *time.Ticker // periodic ticker
	loaded                       chan bool
	shutdown                     chan bool
	forceReload                  chan bool
	featureFlags                 []FeatureFlag
	personalApiKey               string
	projectApiKey                string
	Errorf                       func(format string, args ...interface{})
	Endpoint                     string
	http                         http.Client
	mutex                        sync.RWMutex
	fetchedFlagsSuccessfullyOnce bool
}

type FeatureFlag struct {
	Key               string `json:"key"`
	IsSimpleFlag      bool   `json:"is_simple_flag"`
	RolloutPercentage *uint8 `json:"rollout_percentage"`
	Active            bool   `json:"active"`
	Filters           Filter `json:"filters"`
}

type Filter struct {
	AggregationGroupTypeIndex *uint8          `json:"aggregation_group_type_index"`
	Groups                    []PropertyGroup `json:"groups"`
	Multivariate              *Variants       `json:"multivariate"`
}

type Variants struct {
	Variants []FlagVariant `json:"variants"`
}

type FlagVariant struct {
	Key               string `json:"key"`
	Name              string `json:"name"`
	RolloutPercentage *uint8 `json:"rollout_percentage"`
}
type PropertyGroup struct {
	Properties        []Property `json:"properties"`
	RolloutPercentage *uint8     `json:"rollout_percentage"`
}

type Property struct {
	Key      string      `json:"key"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value"`
	Type     string      `json:"type"`
}

type FlagVariantMeta struct {
	ValueMin float64
	ValueMax float64
	Key      string
}

type FeatureFlagsResponse struct {
	Results []FeatureFlag `json:"results"`
}

type DecideRequestData struct {
	ApiKey     string `json:"api_key"`
	DistinctId string `json:"distinct_id"`
	Groups     Groups `json:"groups"`
}

type DecideResponse struct {
	FeatureFlags map[string]interface{} `json:"featureFlags"`
}

func newFeatureFlagsPoller(projectApiKey string, personalApiKey string, errorf func(format string, args ...interface{}), endpoint string, httpClient http.Client, pollingInterval time.Duration) *FeatureFlagsPoller {
	poller := FeatureFlagsPoller{
		ticker:                       time.NewTicker(pollingInterval),
		loaded:                       make(chan bool),
		shutdown:                     make(chan bool),
		forceReload:                  make(chan bool),
		personalApiKey:               personalApiKey,
		projectApiKey:                projectApiKey,
		Errorf:                       errorf,
		Endpoint:                     endpoint,
		http:                         httpClient,
		mutex:                        sync.RWMutex{},
		fetchedFlagsSuccessfullyOnce: false,
	}

	go poller.run()
	return &poller
}

func (poller *FeatureFlagsPoller) run() {
	poller.fetchNewFeatureFlags()

	for {
		select {
		case <-poller.shutdown:
			close(poller.shutdown)
			close(poller.forceReload)
			close(poller.loaded)
			poller.ticker.Stop()
			return
		case <-poller.forceReload:
			poller.fetchNewFeatureFlags()
		case <-poller.ticker.C:
			poller.fetchNewFeatureFlags()
		}
	}
}

func (poller *FeatureFlagsPoller) fetchNewFeatureFlags() {
	personalApiKey := poller.personalApiKey
	requestData := []byte{}
	headers := [][2]string{{"Authorization", "Bearer " + personalApiKey + ""}}
	res, err := poller.request("GET", "api/feature_flag", requestData, headers)
	if err != nil || res.StatusCode != http.StatusOK {
		poller.Errorf("Unable to fetch feature flags", err)
	}
	defer res.Body.Close()
	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		poller.Errorf("Unable to fetch feature flags", err)
		return
	}
	featureFlagsResponse := FeatureFlagsResponse{}
	err = json.Unmarshal([]byte(resBody), &featureFlagsResponse)
	if err != nil {
		poller.Errorf("Unable to unmarshal response from api/feature_flag", err)
		return
	}
	if !poller.fetchedFlagsSuccessfullyOnce {
		poller.loaded <- true
	}
	newFlags := []FeatureFlag{}
	for _, flag := range featureFlagsResponse.Results {
		if flag.Active {
			newFlags = append(newFlags, flag)
		}
	}
	poller.mutex.Lock()
	poller.featureFlags = newFlags
	poller.mutex.Unlock()

}

func (poller *FeatureFlagsPoller) IsFeatureEnabled(key string, distinctId string, defaultResult bool, personProperties Properties, groupProperties Properties) (bool, error) {
	result, err := poller.GetFeatureFlag(key, distinctId, defaultResult, personProperties, groupProperties)
	if err != nil {
		return false, err
	}
	var flagValueString = fmt.Sprintf("%v", result)
	if flagValueString != "false" {
		return true, nil
	}
	return false, nil
}

func (poller *FeatureFlagsPoller) GetFeatureFlag(key string, distinctId string, defaultResult interface{}, personProperties Properties, groupProperties Properties) (interface{}, error) {
	featureFlags := poller.GetFeatureFlags()

	if len(featureFlags) < 1 {
		return defaultResult, nil
	}

	featureFlag := FeatureFlag{Key: ""}

	// avoid using flag for conflicts with Golang's stdlib `flag`
	for _, storedFlag := range featureFlags {
		if key == storedFlag.Key {
			featureFlag = storedFlag
			break
		}
	}

	if featureFlag.Key == "" {
		return defaultResult, nil
	}

	// TODO: handle groups
	matchingVariantOrBool, err := matchFeatureFlagProperties(featureFlag, distinctId, personProperties)

	if err != nil {
		return defaultResult, nil
	}

	if matchingVariantOrBool != nil {
		return matchingVariantOrBool, nil
	}

	return poller.getFeatureFlagVariant(featureFlag, key, distinctId)
}

func getMatchingVariant(flag FeatureFlag, distinctId string) (interface{}, error) {
	lookupTable := getVariantLookupTable(flag)

	for _, variant := range lookupTable {
		minHash, err := _hash(flag.Key, distinctId, "variant")

		if err != nil {
			return nil, err
		}

		maxHash, err := _hash(flag.Key, distinctId, "variant")

		if err != nil {
			return nil, err
		}

		if minHash >= float64(variant.ValueMin) && maxHash < float64(variant.ValueMax) {
			return variant.Key, nil
		}
	}

	return true, nil
}

func getVariantLookupTable(flag FeatureFlag) []FlagVariantMeta {
	lookupTable := []FlagVariantMeta{}
	valueMin := 0.00

	multivariates := flag.Filters.Multivariate

	if multivariates == nil || multivariates.Variants == nil {
		return lookupTable
	}

	for _, variant := range multivariates.Variants {
		valueMax := float64(valueMin) + float64(*variant.RolloutPercentage)/100
		_flagVariantMeta := FlagVariantMeta{ValueMin: float64(valueMin), ValueMax: valueMax, Key: variant.Key}
		lookupTable = append(lookupTable, _flagVariantMeta)
		valueMin = float64(valueMax)
	}

	return lookupTable

}

func matchFeatureFlagProperties(flag FeatureFlag, distinctId string, properties Properties) (interface{}, error) {
	conditions := flag.Filters.Groups

	for _, condition := range conditions {
		isMatch, err := isConditionMatch(flag, distinctId, condition, properties)

		if err != nil {
			return nil, err
		}

		if isMatch {
			return getMatchingVariant(flag, distinctId)
		}
	}

	return false, nil
}

func isConditionMatch(flag FeatureFlag, distinctId string, condition PropertyGroup, properties Properties) (bool, error) {
	if len(condition.Properties) > 0 {
		for _, prop := range condition.Properties {

			isMatch, err := matchProperty(prop, properties)
			if err != nil {
				return false, err
			}

			if !isMatch {
				return false, nil
			}
		}

		if condition.RolloutPercentage != nil {
			return true, nil
		}
	}

	if condition.RolloutPercentage != nil {
		return checkIfSimpleFlagEnabled(flag.Key, distinctId, *condition.RolloutPercentage)
	}

	return true, nil
}

func matchProperty(property Property, properties Properties) (bool, error) {
	key := property.Key
	operator := property.Operator
	value := property.Value

	if _, ok := properties[key]; !ok {
		errMessage := "Can't match properties without a given property value"
		return false, errors.New(errMessage)
	}

	if operator == "is_not_set" {
		errMessage := "Can't match properties with operator is_not_set"
		return false, errors.New(errMessage)
	}

	override_value, _ := properties[key]

	if operator == "exact" {
		switch t := value.(type) {
		case []interface{}:
			return contains(t, override_value), nil
		default:
			return value == override_value, nil
		}
	}

	if operator == "is_not" {
		switch t := value.(type) {
		case []interface{}:
			return !contains(t, override_value), nil
		default:
			return value != override_value, nil
		}
	}

	if operator == "is_set" {
		return true, nil
	}

	if operator == "icontains" {
		return strings.Contains(strings.ToLower(override_value.(string)), strings.ToLower(value.(string))), nil
	}

	if operator == "not_icontains" {
		return !strings.Contains(strings.ToLower(override_value.(string)), strings.ToLower(value.(string))), nil
	}

	if operator == "regex" {
		r, err := regexp.Compile(value.(string))

		if err != nil {
			errMessage := "Invalid regex"
			return false, errors.New(errMessage)
		}

		match := r.MatchString(override_value.(string))

		if match {
			return true, nil
		} else {
			return false, nil
		}
	}

	if operator == "not_regex" {
		r, err := regexp.Compile(value.(string))

		if err != nil {
			errMessage := "Invalid regex"
			return false, errors.New(errMessage)
		}

		match := r.MatchString(override_value.(string))

		if !match {
			return true, nil
		} else {
			return false, nil
		}
	}

	if operator == "gt" {
		valueOrderable, overrideValueOrderable, err := validateOrderable(value, override_value)
		if err != nil {
			return false, err
		}

		return overrideValueOrderable > valueOrderable, nil
	}

	if operator == "lt" {
		valueOrderable, overrideValueOrderable, err := validateOrderable(value, override_value)
		if err != nil {
			return false, err
		}

		return overrideValueOrderable < valueOrderable, nil
	}

	if operator == "gte" {
		valueOrderable, overrideValueOrderable, err := validateOrderable(value, override_value)
		if err != nil {
			return false, err
		}

		return overrideValueOrderable >= valueOrderable, nil
	}

	if operator == "lte" {
		valueOrderable, overrideValueOrderable, err := validateOrderable(value, override_value)
		if err != nil {
			return false, err
		}

		return overrideValueOrderable <= valueOrderable, nil
	}

	return false, nil

}

func validateOrderable(firstValue interface{}, secondValue interface{}) (float64, float64, error) {
	convertedFirstValue, err := interfaceToFloat(firstValue)

	if err != nil {
		errMessage := "Value 1 is not orderable"
		return 0, 0, errors.New(errMessage)
	}
	convertedSecondValue, err := interfaceToFloat(secondValue)

	if err != nil {
		errMessage := "Value 2 is not orderable"
		return 0, 0, errors.New(errMessage)
	}

	return convertedFirstValue, convertedSecondValue, nil

}

func interfaceToFloat(val interface{}) (float64, error) {

	var i float64
	switch t := val.(type) {
	case int:
		i = float64(t)
	case int8:
		i = float64(t)
	case int16:
		i = float64(t)
	case int32:
		i = float64(t)
	case int64:
		i = float64(t)
	case float32:
		i = float64(t)
	case float64:
		i = float64(t)
	case uint8:
		i = float64(t)
	case uint16:
		i = float64(t)
	case uint32:
		i = float64(t)
	case uint64:
		i = float64(t)
	default:
		errMessage := "Argument not orderable"
		return 0.0, errors.New(errMessage)
	}

	return i, nil
}

func contains(s []interface{}, e interface{}) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func (poller *FeatureFlagsPoller) isSimpleFlagEnabled(key string, distinctId string, rolloutPercentage uint8) (bool, error) {
	isEnabled, err := checkIfSimpleFlagEnabled(key, distinctId, rolloutPercentage)
	if err != nil {
		errMessage := "Error converting string to int"
		poller.Errorf(errMessage)
		return false, errors.New(errMessage)
	}
	return isEnabled, nil
}

// extracted as a regular func for testing purposes
func checkIfSimpleFlagEnabled(key string, distinctId string, rolloutPercentage uint8) (bool, error) {
	val, err := _hash(key, distinctId, "")

	if err != nil {
		return false, err
	}

	return val <= float64(rolloutPercentage)/100, nil
}

func _hash(key string, distinctId string, salt string) (float64, error) {
	hash := sha1.New()
	hash.Write([]byte("" + key + "." + distinctId + "" + salt))
	digest := hash.Sum(nil)
	hexString := fmt.Sprintf("%x\n", digest)[:15]

	value, err := strconv.ParseInt(hexString, 16, 64)
	if err != nil {
		return 0, err
	}

	return float64(value) / LONG_SCALE, nil

}

func (poller *FeatureFlagsPoller) GetFeatureFlags() []FeatureFlag {
	// ensure flags are loaded on the first call
	if !poller.fetchedFlagsSuccessfullyOnce {
		<-poller.loaded
	}

	poller.mutex.RLock()

	defer poller.mutex.RUnlock()

	return poller.featureFlags
}

func (poller *FeatureFlagsPoller) request(method string, endpoint string, requestData []byte, headers [][2]string) (*http.Response, error) {

	url, err := url.Parse(poller.Endpoint + "/" + endpoint + "")

	if err != nil {
		poller.Errorf("creating url - %s", err)
	}
	searchParams := url.Query()

	if method == "GET" {
		searchParams.Add("token", poller.projectApiKey)
	}
	url.RawQuery = searchParams.Encode()

	req, err := http.NewRequest(method, url.String(), bytes.NewReader(requestData))
	if err != nil {
		poller.Errorf("creating request - %s", err)
	}

	version := getVersion()

	req.Header.Add("User-Agent", "posthog-go (version: "+version+")")
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Content-Length", fmt.Sprintf("%d", len(requestData)))

	for _, header := range headers {
		req.Header.Add(header[0], header[1])
	}

	res, err := poller.http.Do(req)

	if err != nil {
		poller.Errorf("sending request - %s", err)
	}

	return res, err
}

func (poller *FeatureFlagsPoller) ForceReload() {
	poller.forceReload <- true
}

func (poller *FeatureFlagsPoller) shutdownPoller() {
	poller.shutdown <- true
}

func (poller *FeatureFlagsPoller) getFeatureFlagVariants(distinctId string, groups Groups) (map[string]interface{}, error) {
	errorMessage := "Failed when getting flag variants"
	requestDataBytes, err := json.Marshal(DecideRequestData{
		ApiKey:     poller.projectApiKey,
		DistinctId: distinctId,
		Groups:     groups,
	})
	headers := [][2]string{{"Authorization", "Bearer " + poller.personalApiKey + ""}}
	if err != nil {
		errorMessage = "unable to marshal decide endpoint request data"
		poller.Errorf(errorMessage)
		return nil, errors.New(errorMessage)
	}
	res, err := poller.request("POST", "decide/?v=2", requestDataBytes, headers)
	if err != nil || res.StatusCode != http.StatusOK {
		errorMessage = "Error calling /decide/"
		poller.Errorf(errorMessage)
		return nil, errors.New(errorMessage)
	}
	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		errorMessage = "Error reading response from /decide/"
		poller.Errorf(errorMessage)
		return nil, errors.New(errorMessage)
	}
	defer res.Body.Close()
	decideResponse := DecideResponse{}
	err = json.Unmarshal([]byte(resBody), &decideResponse)
	if err != nil {
		errorMessage = "Error parsing response from /decide/"
		poller.Errorf(errorMessage)
		return nil, errors.New(errorMessage)
	}

	return decideResponse.FeatureFlags, nil
}

func (poller *FeatureFlagsPoller) getFeatureFlagVariant(featureFlag FeatureFlag, key string, distinctId string) (interface{}, error) {
	var result interface{} = false

	if featureFlag.IsSimpleFlag {

		// json.Unmarshal will convert JSON `null` to a nullish value for each type
		// which is 0 for uint. However, our feature flags should have rolloutPercentage == 100
		// if it is set to `null`. Having rollout percentage be a pointer and deferencing it
		// here allows its value to be `nil` following json.Unmarhsal, so we can appropriately
		// set it to 100
		rolloutPercentage := uint8(100)
		if featureFlag.RolloutPercentage != nil {
			rolloutPercentage = *featureFlag.RolloutPercentage
		}
		var err error
		result, err = poller.isSimpleFlagEnabled(key, distinctId, rolloutPercentage)
		if err != nil {
			return false, err
		}
	} else {
		featureFlagVariants, variantErr := poller.getFeatureFlagVariants(distinctId, nil)

		if variantErr != nil {
			return false, variantErr
		}

		for flagKey, flagValue := range featureFlagVariants {
			var flagValueString = fmt.Sprintf("%v", flagValue)
			if key == flagKey && flagValueString != "false" {
				result = flagValueString
				break
			}
		}
		return result, nil
	}
	return result, nil
}
