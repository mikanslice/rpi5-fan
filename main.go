// Most of this code is generated from Gemini
// This code is based on https://gist.github.com/serialx/d3213768026b15222ec8b86d92c06819
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"
)

type metrics struct {
	cpuTemp  prometheus.Gauge
	fanSpeed prometheus.Gauge
}

func NewMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		cpuTemp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cpu_temperature_celsius",
			Help: "Current temperature of the CPU.",
		}),
		fanSpeed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fan_speed_percent",
			Help: "Current speed of the fan.",
		}),
	}
	reg.MustRegister(m.cpuTemp)
	reg.MustRegister(m.fanSpeed)
	return m
}

func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"status\": \"OK\"}")
}

func getTemp() float64 {
	f, err := os.Open("/host-sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		log.Printf("Failed to open thermal file: %v", err)
		return 0
	}
	defer f.Close()

	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		log.Printf("Failed to read thermal file: %v", err)
		return 0
	}

	tempStr := strings.TrimSpace(string(buf[:n]))
	rawTemp, err := strconv.ParseFloat(tempStr, 64)
	if err != nil {
		log.Printf("Failed to parse temperature: %v", err)
		return 0
	}
	return rawTemp / 1000.0
}

// --- Fan Controller (RP1 MMIO) ---

type FanController struct {
	mu       sync.Mutex
	mmioData []byte
	fanOn    bool
}

var fanCurve = [][2]float64{
	{60, 25},
	{70, 50},
	{80, 100},
}

const (
	tempOff    = 55.0
	rangeTicks = 41566 / 20

	clkPwm1Ctrl    = 0x18084
	clkPwm1DivInt  = 0x18088
	clkPwm1DivFrac = 0x1808c
	clkPwm1Sel     = 0x18090
	clkCtrlEnable  = 1 << 11
	auxsrcXosc     = 2

	gpio45Ctrl     = 0xd0000 + 0x8000 + 11*8 + 4
	pwmFuncsel     = 0

	pwm1           = 0x9c000
	ch             = 3
	globCtrl       = pwm1 + 0x000
	chanCtrl       = pwm1 + 0x014 + ch*16
	rangeReg       = pwm1 + 0x018 + ch*16
	dutyReg        = pwm1 + 0x020 + ch*16
)

func NewFanController(resourcePath string) (*FanController, error) {
	if resourcePath == "" {
		resourcePath = "/host-sys/bus/pci/devices/0002:01:00.0/resource1"
	}

	fd, err := unix.Open(resourcePath, unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open resource file: %v", err)
	}
	defer unix.Close(fd)

	mmio, err := unix.Mmap(fd, 0, 4*1024*1024, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("failed to mmap resource: %v", err)
	}

	fc := &FanController{mmioData: mmio}
	fc.initPWM()
	return fc, nil
}

func (fc *FanController) read32(offset int) uint32 {
	return binary.LittleEndian.Uint32(fc.mmioData[offset:])
}

func (fc *FanController) write32(offset int, val uint32) {
	binary.LittleEndian.PutUint32(fc.mmioData[offset:], val)
}

func (fc *FanController) initPWM() {
	clkCtrl := fc.read32(clkPwm1Ctrl)
	fc.write32(clkPwm1Ctrl, clkCtrl & ^uint32(clkCtrlEnable))
	fc.write32(clkPwm1DivInt, 1)
	fc.write32(clkPwm1DivFrac, 0)
	clkCtrl = fc.read32(clkPwm1Ctrl)
	clkCtrl &= ^uint32(0x3E1)
	clkCtrl |= (auxsrcXosc << 5)
	clkCtrl |= 1
	clkCtrl |= clkCtrlEnable
	fc.write32(clkPwm1Ctrl, clkCtrl)
	fc.write32(clkPwm1Sel, 1<<1)

	ctrl := fc.read32(gpio45Ctrl)
	if (ctrl & 0x1f) != pwmFuncsel {
		fc.write32(gpio45Ctrl, (ctrl & ^uint32(0x1f)) | pwmFuncsel)
	}
}

func (fc *FanController) SetSpeed(pct float64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.mmioData == nil {
		return
	}

	duty := int64(rangeTicks) * int64(pct) / 100
	fc.write32(chanCtrl, 0x109)
	fc.write32(dutyReg, uint32(duty))
	fc.write32(rangeReg, uint32(rangeTicks))

	glob := fc.read32(globCtrl)
	glob |= (1 << ch)
	fc.write32(globCtrl, glob)
	glob |= (1 << 31)
	fc.write32(globCtrl, glob)
}

func (fc *FanController) CalcSpeed(temp float64) float64 {
	if temp <= tempOff {
		fc.fanOn = false
		return 0
	}
	if temp < fanCurve[0][0] {
		if !fc.fanOn {
			return 0
		}
		return fanCurve[0][1]
	}
	if temp >= fanCurve[len(fanCurve)-1][0] {
		fc.fanOn = true
		return fanCurve[len(fanCurve)-1][1]
	}

	fc.fanOn = true
	for i := 0; i < len(fanCurve)-1; i++ {
		t0, p0 := fanCurve[i][0], fanCurve[i][1]
		t1, p1 := fanCurve[i+1][0], fanCurve[i+1][1]
		if temp >= t0 && temp < t1 {
			return p0 + (p1-p0)*(temp-t0)/(t1-t0)
		}
	}
	return fanCurve[len(fanCurve)-1][1]
}

func (fc *FanController) AdjustFanSpeed(temp float64) float64 {
	pct := fc.CalcSpeed(temp)
	fc.SetSpeed(pct)
	return pct
}

// ----------------------------------------

func main() {
	fanCtrl, err := NewFanController(os.Getenv("RESOURCE_PATH"))
	if err != nil {
		log.Printf("Warning: Failed to initialize hardware PWM: %v. Running in dummy mode.", err)
	} else {
		defer func() {
			log.Println("[fan-control] Shutting down, setting fan to 100%")
			fanCtrl.SetSpeed(100)
		}()
	}

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.cpuTemp.Set(120)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		var lastFanSpeed float64

		for range ticker.C {
			currentTemp := getTemp()
			m.cpuTemp.Set(currentTemp)

			var currentFanSpeed float64
			if fanCtrl != nil {
				currentFanSpeed = fanCtrl.AdjustFanSpeed(currentTemp)
			} else {
				currentFanSpeed = 0
			}

			m.fanSpeed.Set(currentFanSpeed)
			if currentFanSpeed != lastFanSpeed {
				log.Printf("[FanSpeed Changed] Temp: %.1f°C, Fan: %.1f%%\n", currentTemp, currentFanSpeed)
				lastFanSpeed = currentFanSpeed
			}
		}
	}()

	log.Println("Starting metrics server on :8080...")
	http.HandleFunc("/healthz", health)
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	log.Fatal(http.ListenAndServe(":8080", nil))
}