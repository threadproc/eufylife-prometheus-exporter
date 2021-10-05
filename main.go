package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

const eufyClientId = "eufy-app"
const eufyClientSecret = "8FHf22gaTKu7MZXqz5zytw"
const eufyAPIBase = "https://home-api.eufylife.com/v1/"
const configPath = "config.json"

var metricNames = []string{"bmi",
	"bmr",
	"body_age",
	"body_fat",
	"body_fat_mass",
	"bone",
	"bone_mass",
	"muscle",
	"muscle_mass",
	"protein_ratio",
	"visceral_fat",
	"water",
	"weight",
}

type config struct {
	AccessToken string
	Expires     int64
	Email       string
	Password    string
	LastCheck   int64
	LastDatum   map[string]*eufyDataPoint
	ListenAddr  string
}

func loadConfig() (*config, error) {
	bs, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	conf := &config{
		ListenAddr: "0.0.0.0:8080",
	}
	if err := json.Unmarshal(bs, conf); err != nil {
		return nil, err
	}

	return conf, nil
}

type authBody struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Email        string `json:"email"`
	Password     string `json:"password"`
}

type authResponse struct {
	ResultCode  int           `json:"res_code"`
	Message     string        `json:"message"`
	AccessToken string        `json:"access_token"`
	ExpiresIn   int           `json:"expires_in"`
	Devices     []*eufyDevice `json:"devices"`
}

type dataResponse struct {
	ResultCode int              `json:"res_code"`
	Message    string           `json:"message"`
	Data       []*eufyDataPoint `json:"data"`
}

type eufyDevice struct {
	ID string `json:"id"`
}

type eufyDataPoint struct {
	DeviceID   string                 `json:"device_id"`
	CreateTime int64                  `json:"create_time"`
	UpdateTime int64                  `json:"update_time"`
	ScaleData  map[string]interface{} `json:"scale_data"`
}

func saveConfig(conf *config) error {
	confbs, err := json.MarshalIndent(conf, "", "    ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(configPath, confbs, 0644)
}

func auth(conf *config) error {
	body := &authBody{
		ClientID:     eufyClientId,
		ClientSecret: eufyClientSecret,
		Email:        conf.Email,
		Password:     conf.Password,
	}
	bodybs, err := json.MarshalIndent(body, "", "    ")
	if err != nil {
		return err
	}

	resp, err := http.Post(eufyAPIBase+"user/v2/email/login", "application/json", bytes.NewBuffer(bodybs))
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return errors.New("non-200 response from API")
	}

	bodybs, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	authr := &authResponse{}
	if err := json.Unmarshal(bodybs, authr); err != nil {
		return err
	}

	if authr.ResultCode != 1 {
		return errors.New(strconv.Itoa(authr.ResultCode) + ": " + authr.Message)
	}

	// save access token
	expiry := time.Now().Unix() + int64(authr.ExpiresIn) - (24 * 60 * 60)
	conf.Expires = expiry
	conf.AccessToken = authr.AccessToken

	// save the config
	return saveConfig(conf)
}

func getLatestData(conf *config) (map[string]*eufyDataPoint, error) {
	// get list of scales
	req, _ := http.NewRequest("GET", eufyAPIBase+"device", nil)
	req.Header.Set("token", conf.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.WithError(err).Error("could not enumerate devices")
		return nil, err
	}
	if resp.StatusCode != 200 {
		log.Error("non-200 status code for device listing")
		return nil, errors.New("non-200 status code for device listing")
	}
	bodybs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("could not read body for device list")
		return nil, err
	}
	// we can reuse this type
	devicesBody := &authResponse{}
	if err := json.Unmarshal(bodybs, devicesBody); err != nil {
		log.WithError(err).Error("could not parse device list body")
		return nil, err
	}
	if devicesBody.ResultCode != 1 {
		err := errors.New(strconv.Itoa(devicesBody.ResultCode) + ": " + devicesBody.Message)
		log.WithError(err).Error("non-1 result code for device list")
		return nil, err
	}

	for _, device := range devicesBody.Devices {
		log.Info("Found device with ID ", device.ID)

		// get history data
		req, _ := http.NewRequest("GET", eufyAPIBase+"device/"+device.ID+"/data?after="+strconv.Itoa(int(conf.LastCheck)), nil)
		req.Header.Set("token", conf.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.WithError(err).Error("could not get history for device")
			return nil, err
		}
		if resp.StatusCode != 200 {
			log.Error("non-200 status code for device listing")
			return nil, errors.New("non-200 status code for device listing")
		}
		bodybs, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.WithError(err).Error("could not read body for device history")
			return nil, err
		}
		deviceData := &dataResponse{}
		if err := json.Unmarshal(bodybs, deviceData); err != nil {
			log.WithError(err).Error("could not parse device data body")
			return nil, err
		}
		if deviceData.ResultCode != 1 {
			err := errors.New(strconv.Itoa(deviceData.ResultCode) + ": " + deviceData.Message)
			log.WithError(err).Error("non-1 result code")
			return nil, err
		}

		var datum *eufyDataPoint
		reused := false
		if len(deviceData.Data) < 1 {
			//log.Info("Reusing old datum point")
			reused = true
			if conf.LastDatum == nil {
				conf.LastDatum = map[string]*eufyDataPoint{}
			}
			var ok bool
			datum, ok = conf.LastDatum[device.ID]
			if !ok {
				datum = nil
			}
		} else {
			//log.Info("New datum point!")
			datum = deviceData.Data[0]
		}

		if datum == nil {
			continue
		}

		if !reused {
			if conf.LastDatum == nil {
				conf.LastDatum = map[string]*eufyDataPoint{}
			}
			conf.LastDatum[device.ID] = datum
			conf.LastCheck = time.Now().Unix()
			if err := saveConfig(conf); err != nil {
				log.WithError(err).Error("Failed to save configuration!")
				return nil, err
			}
		}
	}

	return conf.LastDatum, nil
}

func main() {
	conf, err := loadConfig()
	if err != nil {
		log.WithError(err).Fatal("Failed to load configuration!")
	}

	if conf.AccessToken == "" || time.Now().Unix() >= conf.Expires {
		log.Info("No token or expired, authenticating")

		// do the auth
		if err := auth(conf); err != nil {
			log.WithError(err).Fatal("Failed to authenticate!")
		}
	}

	r := mux.NewRouter()
	r.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		data, err := getLatestData(conf)
		if err != nil {
			w.WriteHeader(500)
			return
		}

		for deviceID, datum := range data {

			for _, metric := range metricNames {
				value := fmt.Sprintf("%v", datum.ScaleData[metric])
				if value == "0" {
					continue
				}

				w.Write([]byte(fmt.Sprintf("eufylife_%s{deviceID=\"%s\",email=\"%s\"} %s\n", metric, deviceID, conf.Email, value)))
			}

		}
	})
	log.Info("Starting server on ", conf.ListenAddr)
	log.Fatal(http.ListenAndServe(conf.ListenAddr, r))

}
