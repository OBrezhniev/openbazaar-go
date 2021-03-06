package zend

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/OpenBazaar/openbazaar-go/bitcoin/exchange"
	"golang.org/x/net/proxy"
)

type ExchangeRateProvider struct {
	fetchUrl        string
	cache           map[string]float64
	client          *http.Client
	decoder         ExchangeRateDecoder
	bitcoinProvider *exchange.BitcoinPriceFetcher
}

type ExchangeRateDecoder interface {
	decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error)
}

type OpenBazaarDecoder struct{}
type OkexDecoder struct{}
type CryptopiaDecoder struct{}
type BitfinexDecoder struct{}
type BittrexDecoder struct{}

type ZenCashPriceFetcher struct {
	sync.Mutex
	cache     map[string]float64
	providers []*ExchangeRateProvider
}

func NewZenCashPriceFetcher(dialer proxy.Dialer) *ZenCashPriceFetcher {
	bp := exchange.NewBitcoinPriceFetcher(dialer)
	z := ZenCashPriceFetcher{
		cache: make(map[string]float64),
	}
	dial := net.Dial
	if dialer != nil {
		dial = dialer.Dial
	}
	tbTransport := &http.Transport{Dial: dial}
	client := &http.Client{Transport: tbTransport, Timeout: time.Minute}

	z.providers = []*ExchangeRateProvider{
		{"https://ticker.openbazaar.org/api", z.cache, client, OpenBazaarDecoder{}, nil},
		{"https://bittrex.com/api/v1.1/public/getticker?market=btc-zen", z.cache, client, BittrexDecoder{}, bp},
		{"https://www.cryptopia.co.nz/api/GetMarket/ZEN_BTC", z.cache, client, CryptopiaDecoder{}, bp},
		{"https://www.okex.com/api/v1/ticker.do?symbol=zen_btc", z.cache, client, OkexDecoder{}, bp},
	}
	go z.run()
	return &z
}

func (z *ZenCashPriceFetcher) GetExchangeRate(currencyCode string) (float64, error) {
	z.Lock()
	defer z.Unlock()
	price, ok := z.cache[currencyCode]
	if !ok {
		return 0, errors.New("Currency not tracked")
	}
	return price, nil
}

func (z *ZenCashPriceFetcher) GetLatestRate(currencyCode string) (float64, error) {
	z.fetchCurrentRates()
	z.Lock()
	defer z.Unlock()
	price, ok := z.cache[currencyCode]
	if !ok {
		return 0, errors.New("Currency not tracked")
	}
	return price, nil
}

func (z *ZenCashPriceFetcher) GetAllRates(cacheOK bool) (map[string]float64, error) {
	if !cacheOK {
		err := z.fetchCurrentRates()
		if err != nil {
			return nil, err
		}
	}
	z.Lock()
	defer z.Unlock()
	return z.cache, nil
}

func (z *ZenCashPriceFetcher) UnitsPerCoin() int {
	return exchange.SatoshiPerBTC
}

func (z *ZenCashPriceFetcher) fetchCurrentRates() error {
	z.Lock()
	defer z.Unlock()
	for _, provider := range z.providers {
		err := provider.fetch()
		if err == nil {
			return nil
		}
	}
	log.Error("Failed to fetch zencash exchange rates")
	return errors.New("All exchange rate API queries failed")
}

func (z *ZenCashPriceFetcher) run() {
	z.fetchCurrentRates()
	ticker := time.NewTicker(time.Minute * 15)
	for range ticker.C {
		z.fetchCurrentRates()
	}
}

func (provider *ExchangeRateProvider) fetch() (err error) {
	if len(provider.fetchUrl) == 0 {
		err = errors.New("Provider has no fetchUrl")
		return err
	}
	resp, err := provider.client.Get(provider.fetchUrl)
	if err != nil {
		log.Error("Failed to fetch from "+provider.fetchUrl, err)
		return err
	}
	decoder := json.NewDecoder(resp.Body)
	var dataMap interface{}
	err = decoder.Decode(&dataMap)
	if err != nil {
		log.Error("Failed to decode JSON from "+provider.fetchUrl, err)
		return err
	}
	return provider.decoder.decode(dataMap, provider.cache, provider.bitcoinProvider)
}

func (b OpenBazaarDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	data := dat.(map[string]interface{})

	zen, ok := data["ZEN"]
	if !ok {
		return errors.New(reflect.TypeOf(b).Name() + ".decode: Type assertion failed, missing 'ZEN' field")
	}
	val, ok := zen.(map[string]interface{})
	if !ok {
		return errors.New(reflect.TypeOf(b).Name() + ".decode: Type assertion failed")
	}
	zenRate, ok := val["last"].(float64)
	if !ok {
		return errors.New(reflect.TypeOf(b).Name() + ".decode: Type assertion failed, missing 'last' (float) field")
	}
	for k, v := range data {
		if k != "timestamp" {
			val, ok := v.(map[string]interface{})
			if !ok {
				return errors.New(reflect.TypeOf(b).Name() + ".decode: Type assertion failed")
			}
			price, ok := val["last"].(float64)
			if !ok {
				return errors.New(reflect.TypeOf(b).Name() + ".decode: Type assertion failed, missing 'last' (float) field")
			}
			cache[k] = price * (1 / zenRate)
		}
	}
	return nil
}

func (b BittrexDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	rates, err := bp.GetAllRates(false)
	if err != nil {
		return err
	}
	obj, ok := dat.(map[string]interface{})
	if !ok {
		return errors.New("BittrexDecoder type assertion failure")
	}
	result, ok := obj["result"]
	if !ok {
		return errors.New("BittrexDecoder: field `result` not found")
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return errors.New("BittrexDecoder type assertion failure")
	}
	exRate, ok := resultMap["Last"]
	if !ok {
		return errors.New("BittrexDecoder: field `Last` not found")
	}
	rate, ok := exRate.(float64)
	if !ok {
		return errors.New("BittrexDecoder type assertion failure")
	}

	if rate == 0 {
		return errors.New("Bitcoin-ZenCash price data not available")
	}
	for k, v := range rates {
		cache[k] = v * rate
	}
	return nil
}
func (b CryptopiaDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	rates, err := bp.GetAllRates(false)
	if err != nil {
		return err
	}
	obj, ok := dat.(map[string]interface{})
	if !ok {
		return errors.New("CryptopiaDecoder type assertion failure")
	}
	result, ok := obj["Data"]
	if !ok {
		return errors.New("CryptopiaDecoder: field `Data` not found")
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return errors.New("CryptopiaDecoder type assertion failure")
	}
	exRate, ok := resultMap["LastPrice"]
	if !ok {
		return errors.New("CryptopiaDecoder: field `LastPrice` not found")
	}
	rate, ok := exRate.(float64)
	if !ok {
		return errors.New("CryptopiaDecoder type assertion failure")
	}
	if rate == 0 {
		return errors.New("Bitcoin-ZenCash price data not available")
	}
	for k, v := range rates {
		cache[k] = v * rate
	}
	return nil
}
func (b OkexDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	rates, err := bp.GetAllRates(false)
	if err != nil {
		return err
	}
	obj, ok := dat.(map[string]interface{})
	if !ok {
		return errors.New("OkexDecoder type assertion failure")
	}
	result, ok := obj["result"]
	if !ok {
		return errors.New("OkexDecoder: field `ticker` not found")
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return errors.New("OkexDecoder type assertion failure")
	}
	exRate, ok := resultMap["Last"]
	if !ok {
		return errors.New("OkexDecoder: field `last` not found")
	}
	rate, ok := exRate.(float64)
	if !ok {
		return errors.New("OkexDecoder type assertion failure")
	}
	if rate == 0 {
		return errors.New("Bitcoin-ZenCash price data not available")
	}
	for k, v := range rates {
		cache[k] = v * rate
	}
	return nil
}
