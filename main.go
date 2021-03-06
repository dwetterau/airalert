package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	phone  string
	Sensor string
	Name   string
}

var (
	twilioSID    string
	twilioSecret string
	twilioNum    string
)

func main() {
	port := flag.Int("port", 0, "if non-zero, will launch a website instead.")
	dbFilePath := flag.String("db_file", "", "file to the previous readings")
	flag.StringVar(&twilioSID, "twilio_sid", "", "twilio account sid")
	flag.StringVar(&twilioSecret, "twilio_secret", "", "twilio account secret")
	flag.StringVar(&twilioNum, "twilio_source_num", "", "twilio source phone number")
	flag.Parse()

	var configs []Config
	for i := 0; ; i++ {
		suffix := fmt.Sprintf("_%d", i)
		c := Config{
			phone:  os.Getenv("PHONE" + suffix),
			Sensor: os.Getenv("SENSOR" + suffix),
			Name:   os.Getenv("NAME" + suffix),
		}
		if len(c.phone) == 0 {
			break
		}
		configs = append(configs, c)
	}

	if *port != 0 {
		tmpl, err := template.ParseFiles("src/github.com/dwetterau/airalert/index.html")
		if err != nil {
			panic(err)
		}
		http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			sensor := filepath.Base(req.URL.Path)
			temp, rawPM, err := GetAQI(sensor)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			name := ""
			for _, c := range configs {
				if c.Sensor == sensor {
					name = c.Name
				}
			}
			if len(name) == 0 {
				name = sensor
			}
			pm := RawEPAConverter(rawPM)
			err = tmpl.Execute(w, struct {
				HasName      bool
				Name         string
				Temp         int
				AQI          int
				AQIColor     string
				AQITextColor string
				Configs      []Config
			}{
				HasName:      len(name) > 0,
				Name:         name,
				Temp:         temp,
				AQI:          pm,
				AQIColor:     AQIColor(pm),
				AQITextColor: AQITextColor(pm),
				Configs:      configs,
			})
			if err != nil {
				fmt.Println("error executing template", err.Error())
			}
		})
		if err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", *port), nil); err != nil {
			panic(err)
		}
		return
	}

	db, err := readDB(*dbFilePath)
	if err != nil {
		panic(err)
	}

	for _, c := range configs {
		err = SendAlerts(c, db)
		if err != nil {
			panic(err)
		}
	}
	if err := writeDB(*dbFilePath, db); err != nil {
		panic(err)
	}
}

func SendAlerts(c Config, readings map[string]Reading) error {
	temp, rawPM, err := GetAQI(c.Sensor)
	if err != nil {
		return err
	}
	pm := RawEPAConverter(rawPM)
	lastReading := readings[c.Sensor]
	lastTemp := lastReading.temp
	lastPM := lastReading.pm25

	fmt.Printf(
		"Last measurement for %s: (t: %d, pm: %d). Now: (t: %d, pm: %d)\n",
		c.Sensor,
		lastTemp,
		lastPM,
		temp,
		pm,
	)
	if (lastTemp > 80 && temp < 80) && pm < 100 {
		err = SendText(c.phone, fmt.Sprintf("It's cooling off! Temp: %d AQI: %d", temp, pm))
		if err != nil {
			fmt.Println("warning, failed to send text", err.Error())
		}
	}
	if (lastPM > 100 && pm < 100) && lastTemp < 80 {
		err = SendText(c.phone, fmt.Sprintf("It's nice out! Maybe you can open a window. Temp: %d AQI: %d", temp, pm))
		if err != nil {
			fmt.Println("warning, failed to send text", err.Error())
		}
	}
	if pm > (lastPM + 40) {
		err = SendText(c.phone, fmt.Sprintf("Greetings earthling! The AQI is now: %d", pm))
		if err != nil {
			fmt.Println("warning, failed to send text", err.Error())
		}
	}
	readings[c.Sensor] = Reading{
		temp: temp,
		pm25: pm,
	}
	return nil
}

type Reading struct {
	temp int
	pm25 int
}

func readDB(dbFile string) (map[string]Reading, error) {
	var f *os.File
	var err error
	if _, err = os.Stat(dbFile); os.IsNotExist(err) {
		f, err = os.Create(dbFile)
	} else {
		f, err = os.Open(dbFile)
	}
	if err != nil {
		return nil, err
	}
	dataRaw, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	readings := make(map[string]Reading, 2)
	// Format: unixTSSecs,sensor,Temp,Air
	for _, l := range strings.Split(strings.TrimSpace(string(dataRaw)), "\n") {
		split := strings.Split(l, ",")
		if len(split) < 4 {
			continue
		}
		sensor := split[1]
		rawTemp, _ := strconv.ParseInt(split[2], 10, 64)
		lastPMRaw, _ := strconv.ParseInt(split[3], 10, 64)
		readings[sensor] = Reading{
			temp: int(rawTemp),
			pm25: int(lastPMRaw),
		}
	}
	return readings, nil
}

func writeDB(dbFile string, readings map[string]Reading) error {
	out := ""
	for sensor, reading := range readings {
		out += fmt.Sprintf("%d,%s,%d,%d\n", time.Now().Unix(), sensor, reading.temp, reading.pm25)
	}
	return ioutil.WriteFile(dbFile, []byte(out), 0666)
}

func GetAQI(sensor string) (int, float64, error) {
	resp, err := http.Get("https://www.purpleair.com/json?show=" + sensor)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	rawData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}
	var data AQIResponse
	if err := json.Unmarshal(rawData, &data); err != nil {
		return 0, 0, err
	}
	numPM := 0
	pm := 0.0
	numTemp := 0
	temp := 0
	for _, s := range data.Results {
		if len(s.TempF) > 0 {
			rawTemp, err := strconv.ParseInt(s.TempF, 10, 64)
			if err != nil {
				fmt.Println("warning, could not parse temp:", s.TempF, err.Error())
			} else {
				temp += int(rawTemp)
				numTemp++
			}
		}
		if len(s.Stats) > 0 {
			var stats StatsResponse
			if err := json.Unmarshal([]byte(s.Stats), &stats); err != nil {
				fmt.Println("warning, could not parse pm:", s.Stats, err.Error())
				continue
			}
			v := stats.Avg10m
			pm += v
			numPM++
		}
	}
	if numTemp > 0 {
		temp /= numTemp
	}
	if numPM > 0 {
		pm /= float64(numPM)
	}
	return temp, pm, nil
}

func RawEPAConverter(x float64) int {
	x = math.Round(math.Round(x*10) / 10)
	y := 0.0
	if x <= 15.4 {
		y = 3.247 * x
	} else if x <= 65.4 {
		y = 1.968*(x-15.5) + 51
	} else if x <= 150.4 {
		y = 0.577*(x-65.5) + 151
	} else if x <= 250.4 {
		y = 0.991*(x-150.5) + 201
	} else {
		y = 0.796*(x-250.5) + 301
	}
	return int(math.Round(y))
}

func AQIColor(aqi int) string {
	colors := [][]int{
		{60, 179, 113},
		{255, 255, 102},
		{255, 140, 0},
		{255, 40, 0},
		{128, 0, 128},
	}
	i := 0
	j := 0
	percent := 0.0
	if aqi <= 50 {
		i = 0
		j = 0
	} else if aqi <= 75 {
		i = 0
		j = 1
		percent = (float64(aqi) - 50.0) / 25.0
	} else if aqi <= 125 {
		i = 1
		j = 2
		percent = (float64(aqi) - 75.0) / (125.0 - 75.0)
	} else if aqi <= 175 {
		i = 2
		j = 3
		percent = (float64(aqi) - 125.0) / (175.0 - 125.0)
	} else if aqi <= 250 {
		i = 3
		j = 4
		percent = (float64(aqi) - 175.0) / (250.0 - 175.0)
	} else {
		i = 4
		j = 4
	}
	if i == j {
		return fmt.Sprintf("#%s%s%s", hex(colors[i][0]), hex(colors[i][1]), hex(colors[i][2]))
	}
	return fmt.Sprintf(
		"#%s%s%s",
		hex(inter(colors[i][0], colors[j][0], percent)),
		hex(inter(colors[i][1], colors[j][1], percent)),
		hex(inter(colors[i][2], colors[j][2], percent)),
	)
}

func hex(v int) string {
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return fmt.Sprintf("%02x", v)
}

func inter(a, b int, p float64) int {
	return int(math.Round(float64(a)*(1-p) + float64(b)*p))
}

func AQITextColor(aqi int) string {
	if aqi <= 100 {
		return "black"
	}
	return "white"
}

type AQIResponse struct {
	Results []SensorResponse `json:"results"`
}

type SensorResponse struct {
	PM2_5Value string
	TempF      string `json:"temp_f"`
	Stats      string
}

type StatsResponse struct {
	Current float64 `json:"v"`
	Avg10m  float64 `json:"v1"`
}

func SendText(number, message string) error {
	fmt.Println("Sending message to:", number, message)
	accountSID := twilioSID
	authToken := twilioSecret
	urlStr := "https://api.twilio.com/2010-04-01/Accounts/" + accountSID + "/Messages.json"

	msgData := url.Values{}
	msgData.Set("To", number)
	msgData.Set("From", twilioNum)
	msgData.Set("Body", message)
	msgDataReader := *strings.NewReader(msgData.Encode())

	client := &http.Client{}
	req, err := http.NewRequest("POST", urlStr, &msgDataReader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var data map[string]interface{}
		decoder := json.NewDecoder(resp.Body)
		err := decoder.Decode(&data)
		if err == nil {
			fmt.Println(data["sid"])
		}
		fmt.Println(data)
		return err
	} else {
		fmt.Println(resp.Status)
	}
	return nil
}
