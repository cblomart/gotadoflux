package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cblomart/gotadoflux/config"
	"github.com/cblomart/gotadoflux/tado"

	client "github.com/influxdata/influxdb1-client/v2"
	"github.com/takama/daemon"
)

var (
	conf         = &config.Config{}
	tadoclient   *tado.Tado
	zones        = map[int]map[string]string{}
	influx       client.Client
	points       = []*client.Point{}
	dependencies = []string{}
	lastSync     = time.Time{}
)

// function to collect data
func collect() {
	log.Println("retrieving data from tado")
	for _, home := range conf.Collect {
		states, err := tadoclient.GetZoneStates(home.Id)
		if err != nil {
			log.Printf("could not get zone states for %s: %s", home.Name, err)
			continue
		}
		// check that we have zones for the home
		_, homefound := zones[home.Id]
		// check that all states have a known zone
		allzonesfound := true
		if homefound {
			for zoneId := range states.ZoneStates {
				if _, zonefound := zones[home.Id][zoneId]; !zonefound {
					allzonesfound = false
					break
				}
			}
		}
		// refresh zones information if needed
		if !homefound || !allzonesfound {
			log.Println("refreshing zone informations")
			newZones, err := tadoclient.GetZones(home.Id)
			if err != nil {
				log.Printf("could not get zones for %s: %s", home.Name, err)
				continue
			}
			zones[home.Id] = map[string]string{}
			for _, zone := range newZones {
				zones[home.Id][strconv.Itoa(zone.Id)] = zone.Name
			}
		}
		for zoneId, state := range states.ZoneStates {
			zoneName := zones[home.Id][zoneId]
			if state.SensorDataPoints.InsideTemperature == nil {
				continue
			}
			if state.SensorDataPoints.InsideTemperature.Timestamp.Before(lastSync) {
				log.Printf("Skipping %s in %s: data before last sync", zoneName, home.Name)
				continue
			}
			name := fmt.Sprintf("%s.%s", home.Name, zoneName)
			powerPc := float32(0.0)
			if state.ActivityDataPoints.AcPower != nil {
				if strings.ToUpper(*state.ActivityDataPoints.AcPower.Value) == "ON" {
					powerPc = 100.0
				}
			}
			if state.ActivityDataPoints.HeatingPower != nil {
				powerPc = *state.ActivityDataPoints.HeatingPower.Percentage
			}
			point, err := client.NewPoint(
				name,
				map[string]string{
					"homeId":   strconv.Itoa(home.Id),
					"homeName": home.Name,
					"zoneId":   zoneId,
					"zoneName": zoneName,
					"source":   "tado",
				},
				map[string]interface{}{
					"temperature": *state.SensorDataPoints.InsideTemperature.Celsius,
					"humidity":    *state.SensorDataPoints.Humidity.Percentage,
					"power":       powerPc,
				},
				*state.SensorDataPoints.InsideTemperature.Timestamp,
			)
			if err != nil {
				log.Printf("could not create point for %s", name)
			}
			points = append(points, point)
		}
	}
	bps, err := client.NewBatchPoints(client.BatchPointsConfig{
		Precision: "s",
		Database:  conf.Influx.Database,
	})
	bps.AddPoints(points)
	if err != nil {
		log.Println("cloud not create batchpoints")
		return
	}
	err = influx.Write(bps)
	if err != nil {
		log.Printf("cloud not write to influx: %s", err)
		return
	}
	lastSync = time.Now()
	log.Printf("written %d points to influx", len(points))
	points = nil
}

// Service has embedded daemon
type Service struct {
	daemon.Daemon
}

func (service *Service) Manage() (string, error) {

	usage := "Usage: goviflux install | remove | start | stop | status"

	// if received any kind of command, do it
	if len(os.Args) > 1 {
		command := os.Args[1]
		switch command {
		case "install":
			return service.Install()
		case "remove":
			return service.Remove()
		case "start":
			return service.Start()
		case "stop":
			return service.Stop()
		case "status":
			return service.Status()
		default:
			return usage, nil
		}
	}

	// create a ticker for duration
	ticker := time.NewTicker(conf.Period.Duration)
	defer ticker.Stop()

	// create a channel for system interupt
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, os.Kill, syscall.SIGTERM)

	// create the tado client
	var err error
	tadoclient, err = tado.ConfigToTado(conf)
	if err != nil {
		return fmt.Sprintf("could not instanciate vicare client"), err
	}

	// preparing the influx connection
	influx, err = client.NewHTTPClient(client.HTTPConfig{
		Addr:     conf.Influx.Url,
		Username: conf.Influx.Username,
		Password: conf.Influx.Password,
	})
	if err != nil {
		return fmt.Sprintf("could not instanciate the influx client"), err
	}
	_, ver, err := influx.Ping(5 * time.Second)
	if err != nil {
		return fmt.Sprintf("test connection to influx failed"), err
	}
	log.Printf("connected to influx %s (%s)", conf.Influx.Url, ver)
	defer influx.Close()

	//initial collection
	collect()

	for {
		select {
		case <-ticker.C:
			collect()
		case killSignal := <-interrupt:
			log.Println("Got signal:", killSignal)
			if killSignal == os.Interrupt {
				return "Daemon was interrupted by system signal", nil
			}
			return "Daemon was killed", nil
		}
	}
}

func main() {
	log.Println("Tado logger to influx db")
	// initialize config
	// find config file location
	basename := path.Base(os.Args[0])
	configname := strings.TrimSuffix(basename, filepath.Ext(basename))
	location := fmt.Sprintf("/etc/%s.json", configname)
	if _, err := os.Stat(location); err != nil {
		location = fmt.Sprintf("%s.json", configname)
		if _, err := os.Stat(location); err != nil {
			log.Fatalf("no configuraiton file in '.' or '/etc'")
		}
	}

	// read the configuration
	file, err := os.Open(location)
	if err != nil {
		log.Fatalf("could not open configuration file: %s", location)
	}
	jsondec := json.NewDecoder(file)
	err = jsondec.Decode(conf)
	if err != nil {
		log.Fatalf("could not decode configuration file: %s", location)
	}

	// check token cache path
	if len(conf.RefreshTokenPath) == 0 {
		conf.RefreshTokenPath = fmt.Sprintf("%s.token", configname)
	}

	// create token cache if file does not exist
	refreshTokenPath, err := os.Stat(conf.RefreshTokenPath)
	if os.IsNotExist(err) {
		file, err := os.Create(conf.RefreshTokenPath)
		if err != nil {
			log.Fatalf("could not create file: %s", conf.RefreshTokenPath)
		}
		err = file.Chmod(os.FileMode(int(0600)))
		if err != nil {
			log.Fatalf("could not set file mode to 0600: %s", conf.RefreshTokenPath)
		}
		file.Close()
	} else if refreshTokenPath.Mode() != os.FileMode(int(0600)) && runtime.GOOS != "windows" {
		// on windows token will be protected by DPAPI
		log.Fatalf("token cache should have 0600 mode: %s", conf.RefreshTokenPath)
	}

	// create the daemon
	srv, err := daemon.New("gotadoflux", "Tado logger to influx", daemon.SystemDaemon, dependencies...)
	if err != nil {
		log.Println("Error: ", err)
		os.Exit(1)
	}
	service := &Service{srv}
	status, err := service.Manage()
	if err != nil {
		log.Println("Error: ", err)
		os.Exit(1)
	}
	log.Println(status)
}
