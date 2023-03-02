package tado

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/cblomart/gotadoflux/config"
)

const (
	TADO_TOKEN        = "https://auth.tado.com/oauth/token"
	TADO_CLIENTID     = "tado-web-app"
	TADO_CLIENTSECRET = "wZaRN7rpjn3FoNyF5IFuxg9uMzYJcvOoQ8QWiIqS3hfk6gLhVlG57j5YNoZL2Rtc"
	TADO_SCOPE        = "home.user"
	TADO_API          = "https://my.tado.com/api/v2"
)

type TadoError struct {
	Error       string
	Description string `json:"error_description"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Jti          string `json:"jti"`
}

type Home struct {
	Id   int
	Name string
}

type MeResponse struct {
	Homes []*Home
}

type Zone struct {
	Id   int
	Name string
}

type ZoneStatesResponse struct {
	ZoneStates map[string]ZoneState
}

type ZoneState struct {
	SensorDataPoints   SensorDataPoints
	ActivityDataPoints ActivityDataPoints
}

type ActivityDataPoints struct {
	AcPower      *AcPower      `json:",omitempty"`
	HeatingPower *HeatingPower `json:",omitempty"`
}

type AcPower struct {
	Value     *string    `json:",omitempty"`
	Timestamp *time.Time `json:",omitempty"`
}

type HeatingPower struct {
	Percentage *float32   `json:",omitempty"`
	Timestamp  *time.Time `json:",omitempty"`
}

type SensorDataPoints struct {
	InsideTemperature *Temperature `json:",omitempty"`
	Humidity          *Humidity    `json:",omitempty"`
}

type Temperature struct {
	Celsius   *float32   `json:",omitempty"`
	Timestamp *time.Time `json:",omitempty"`
}

type Humidity struct {
	Percentage *float32   `json:",omitempty"`
	Timestamp  *time.Time `json:",omitempty"`
}

type Tado struct {
	Username         string
	password         string
	refreshTokenPath string
	accessToken      string
	expires          time.Time
	refresh          time.Time
	client           *http.Client
}

func NewTado(username, password, refreshTokenPath string) (*Tado, error) {
	return &Tado{Username: username, password: password, refreshTokenPath: refreshTokenPath, client: &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func ConfigToTado(c *config.Config) (*Tado, error) {
	return NewTado(c.Username, c.Password, c.RefreshTokenPath)
}

func (t *Tado) SaveRefreshToken(token string) error {
	if len(token) == 0 {
		return fmt.Errorf("Cannot save empty refresh token")
	}
	encToken := TokenEncrypt(token)
	err := ioutil.WriteFile(t.refreshTokenPath, []byte(encToken), os.FileMode(int(0666)))
	if err != nil {
		return err
	}
	return nil
}

func (t *Tado) GetRefreshToken() (string, error) {
	fileInfo, err := os.Stat(t.refreshTokenPath)
	if err != nil {
		return "", err
	}
	if fileInfo.Mode() != os.FileMode(int(0600)) && runtime.GOOS != "windows" {
		return "", fmt.Errorf("refresh token cache is not properly protected")
	}
	if fileInfo.ModTime().Add(180 * 25 * time.Hour).Before(time.Now()) {
		return "", nil
	}
	if fileInfo.Size() == 0 {
		return "", nil
	}
	token, err := ioutil.ReadFile(t.refreshTokenPath)
	if err != nil {
		return "", err
	}
	return TokenDecrypt(string(token)), nil
}

func (t *Tado) HasRefreshToken() bool {
	fileInfo, err := os.Stat(t.refreshTokenPath)
	if err != nil {
		return false
	}
	if fileInfo.Mode() != os.FileMode(int(0600)) && runtime.GOOS != "windows" {
		return false
	}
	if fileInfo.ModTime().Add(180 * 25 * time.Hour).Before(time.Now()) {
		return false
	}
	if fileInfo.Size() == 0 {
		return false
	}
	return true
}

func (t *Tado) AquireToken() error {
	// set parameters for token request
	params := url.Values{}
	params.Set("client_id", TADO_CLIENTID)
	params.Set("client_secret", TADO_CLIENTSECRET)
	params.Set("scope", TADO_SCOPE)
	params.Set("grant_type", "password")
	params.Set("username", t.Username)
	params.Set("password", t.password)
	// token request
	tokenrequest, err := http.NewRequest("POST", TADO_TOKEN, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	tokenrequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenresponse, err := t.client.Do(tokenrequest)
	if err != nil {
		return err
	}
	// check for error
	if tokenresponse.StatusCode/100 >= 4 {
		tadoError := &TadoError{}
		json.NewDecoder(tokenresponse.Body).Decode(tadoError)
		return fmt.Errorf("could not get token: %s", tadoError.Description)
	}
	tokens := &TokenResponse{}
	json.NewDecoder(tokenresponse.Body).Decode(tokens)
	err = t.SaveRefreshToken(tokens.RefreshToken)
	if err != nil {
		return fmt.Errorf("Could not save refresh token")
	}
	t.expires = time.Now().Add(time.Second * time.Duration(tokens.ExpiresIn))
	t.refresh = time.Now().Add(time.Second * time.Duration(tokens.ExpiresIn/2))
	t.accessToken = tokens.AccessToken
	return nil
}

func (t *Tado) RefreshToken() error {
	// get refresh token
	if !t.HasRefreshToken() {
		return fmt.Errorf("No refresh token")
	}
	refreshToken, err := t.GetRefreshToken()
	if err != nil {
		return err
	}
	// set parameters for token request
	params := url.Values{}
	params.Set("client_id", TADO_CLIENTID)
	params.Set("client_secret", TADO_CLIENTSECRET)
	params.Set("grant_type", "refresh_token")
	params.Set("refresh_token", refreshToken)
	// token request
	tokenrequest, err := http.NewRequest("POST", TADO_TOKEN, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	tokenrequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenresponse, err := t.client.Do(tokenrequest)
	if err != nil {
		return err
	}
	// check for error
	if tokenresponse.StatusCode/100 >= 4 {
		tadoError := &TadoError{}
		json.NewDecoder(tokenresponse.Body).Decode(tadoError)
		return fmt.Errorf("could not get token: %s", tadoError.Description)
	}
	tokens := &TokenResponse{}
	json.NewDecoder(tokenresponse.Body).Decode(tokens)
	err = t.SaveRefreshToken(tokens.RefreshToken)
	if err != nil {
		return fmt.Errorf("Could not save refresh token")
	}
	t.expires = time.Now().Add(time.Second * time.Duration(tokens.ExpiresIn))
	t.refresh = time.Now().Add(time.Second * time.Duration(tokens.ExpiresIn/2))
	t.accessToken = tokens.AccessToken
	return nil
}

func (t *Tado) AuthCheck() error {
	if len(t.accessToken) > 0 {
		// we have an access token
		if t.refresh.After(time.Now()) {
			// access token valid
			return nil
		} else {
			// access token needs to be refreshed
			if t.HasRefreshToken() {
				// we have a refresh token
				err := t.RefreshToken()
				if err != nil && t.expires.Before(time.Now()) {
					// token expired and can't refresh
					return err
				}
				log.Println("Access token refreshed")
			} else {
				// we don't have a refresh token
				err := t.AquireToken()
				if err != nil && t.expires.Before(time.Now()) {
					// token expired and can't aquire a new one
					return err
				}
				log.Println("Access token acquired")
			}
		}
	}
	if len(t.accessToken) == 0 {
		// we don't have an access token
		if t.HasRefreshToken() {
			// we have a refresh token
			err := t.RefreshToken()
			if err != nil {
				return err
			}
			log.Println("New access token from refresh")
		} else {
			// we don't have a refresh token
			err := t.AquireToken()
			if err != nil {
				return err
			}
			log.Println("New access token")
		}
	}
	if len(t.accessToken) == 0 {
		return fmt.Errorf("No access token")
	}
	return nil
}

func (t *Tado) GetHome() (*Home, error) {
	// check authentication
	err := t.AuthCheck()
	if err != nil {
		return nil, err
	}
	// create request
	request, err := http.NewRequest("GET", fmt.Sprintf("%s/me", TADO_API), nil)
	if err != nil {
		return nil, err
	}
	// set authentication
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.accessToken))
	response, err := t.client.Do(request)
	if err != nil {
		return nil, err
	}
	// check for error
	if response.StatusCode/100 >= 4 {
		tadoError := &TadoError{}
		err = json.NewDecoder(response.Body).Decode(&tadoError)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(tadoError.Description)
	}
	// unmarshall response
	me := MeResponse{}
	err = json.NewDecoder(response.Body).Decode(&me)
	if err != nil {
		return nil, err
	}
	return me.Homes[0], nil
	/*
		// read reponse body
		body, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return err
		}
		log.Printf("body: %s", body)
	*/
}

func (t *Tado) GetZoneStates(id int) (*ZoneStatesResponse, error) {
	// check authentication
	err := t.AuthCheck()
	if err != nil {
		return nil, err
	}
	// create request
	request, err := http.NewRequest("GET", fmt.Sprintf("%s/homes/%d/zoneStates", TADO_API, id), nil)
	if err != nil {
		return nil, err
	}
	// set authentication
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.accessToken))
	response, err := t.client.Do(request)
	if err != nil {
		return nil, err
	}
	// check for error
	if response.StatusCode/100 >= 4 {
		tadoError := &TadoError{}
		err = json.NewDecoder(response.Body).Decode(&tadoError)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(tadoError.Description)
	}
	// unmarshall response
	zoneStates := &ZoneStatesResponse{}
	err = json.NewDecoder(response.Body).Decode(zoneStates)
	if err != nil {
		return nil, err
	}
	return zoneStates, nil
}

func (t *Tado) GetZones(id int) ([]Zone, error) {
	// check authentication
	err := t.AuthCheck()
	if err != nil {
		return nil, err
	}
	// create request
	request, err := http.NewRequest("GET", fmt.Sprintf("%s/homes/%d/zones", TADO_API, id), nil)
	if err != nil {
		return nil, err
	}
	// set authentication
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.accessToken))
	response, err := t.client.Do(request)
	if err != nil {
		return nil, err
	}
	// check for error
	if response.StatusCode/100 >= 4 {
		tadoError := &TadoError{}
		err = json.NewDecoder(response.Body).Decode(&tadoError)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(tadoError.Description)
	}
	// unmarshall response
	zones := []Zone{}
	err = json.NewDecoder(response.Body).Decode(&zones)
	if err != nil {
		return nil, err
	}
	return zones, nil
}
