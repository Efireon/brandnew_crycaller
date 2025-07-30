package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ipmi "github.com/u-root/u-root/pkg/ipmi"
)

const VERSION = "3.0.1"

type PowerInfo struct {
	Name         string  `json:"name"`
	Position     string  `json:"position"`     // PSU1, CPU1, 12V, 5V, VDDQ_A, etc.
	PowerType    string  `json:"power_type"`   // Voltage, Current, Power
	Category     string  `json:"category"`     // PSU, CPU, Memory, System, Chipset, Battery, Other
	Value        float64 `json:"value"`        // текущее значение
	Units        string  `json:"units"`        // Volts, Amps, Watts
	MinValue     float64 `json:"min_value"`    // нижний порог
	MaxValue     float64 `json:"max_value"`    // верхний порог
	CriticalMin  float64 `json:"critical_min"` // критический нижний порог
	CriticalMax  float64 `json:"critical_max"` // критический верхний порог
	Status       string  `json:"status"`       // OK, WARNING, CRITICAL, N/A, etc.
	SensorNumber uint8   `json:"sensor_number"`
	RawValue     float64 `json:"raw_value"`
}

type PowerRequirement struct {
	Name             string            `json:"name"`
	PowerType        string            `json:"power_type"`        // Voltage, Current, Power
	Category         string            `json:"category"`          // PSU, CPU, Memory, System
	Positions        []string          `json:"positions"`         // конкретные позиции
	MinValue         float64           `json:"min_value"`         // минимальное значение
	MaxValue         float64           `json:"max_value"`         // максимальное значение
	TolerancePercent float64           `json:"tolerance_percent"` // допустимое отклонение в %
	ExpectedStatus   map[string]string `json:"expected_status"`   // position -> expected status
}

type PowerVisual struct {
	Symbol      string `json:"symbol"`      // ASCII блочный символ ███, ▓▓▓, ░░░
	ShortName   string `json:"short_name"`  // Короткое имя
	Description string `json:"description"` // Описание
	Color       string `json:"color"`       // Цветовой код
}

type RowConfig struct {
	Name  string `json:"name"`  // Отображаемое имя ряда
	Slots string `json:"slots"` // Диапазон слотов, например "1-8", "9-16"
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Включить кастомные ряды
	Rows    []RowConfig `json:"rows"`    // Сами ряды
}

type VisualizationConfig struct {
	CategoryVisuals map[string]PowerVisual `json:"category_visuals"`
	PositionToSlot  map[string]int         `json:"position_to_slot"`
	TotalSlots      int                    `json:"total_slots"`
	SlotWidth       int                    `json:"slot_width"`
	CustomRows      CustomRowsConfig       `json:"custom_rows"`
}

type Config struct {
	PowerRequirements []PowerRequirement  `json:"power_requirements"`
	Visualization     VisualizationConfig `json:"visualization"`
	IPMITimeout       int                 `json:"ipmi_timeout_seconds"`
	RedfishUsername   string              `json:"redfish_username,omitempty"`
	RedfishPassword   string              `json:"redfish_password,omitempty"`
	RedfishTimeout    int                 `json:"redfish_timeout_seconds"`
	RedfishWorkers    int                 `json:"redfish_workers"` // Количество параллельных воркеров
}

type PowerCheckResult struct {
	Status      string // "ok", "warning", "error", "missing"
	Issues      []string
	VoltageOK   bool
	CurrentOK   bool
	PowerOK     bool
	ToleranceOK bool
	StatusOK    bool
	VoltageWarn bool
	CurrentWarn bool
	PowerWarn   bool
	StatusWarn  bool
}

// Redfish API structures
type RedfishChassis struct {
	ID      string             `json:"Id"`
	Name    string             `json:"Name"`
	Thermal RedfishThermalLink `json:"Thermal"`
	Power   RedfishPowerLink   `json:"Power"`
	Sensors RedfishSensorsLink `json:"Sensors,omitempty"`
}

type RedfishThermalLink struct {
	ID string `json:"@odata.id"`
}

type RedfishPowerLink struct {
	ID string `json:"@odata.id"`
}

type RedfishSensorsLink struct {
	ID string `json:"@odata.id"`
}

type RedfishPower struct {
	PowerSupplies []RedfishPowerSupply `json:"PowerSupplies"`
	Voltages      []RedfishVoltage     `json:"Voltages"`
}

type RedfishPowerSupply struct {
	Name             string              `json:"Name"`
	PowerInputWatts  *float64            `json:"PowerInputWatts"`
	PowerOutputWatts *float64            `json:"PowerOutputWatts"`
	LineInputVoltage *float64            `json:"LineInputVoltage"`
	Status           RedfishStatus       `json:"Status"`
	InputRanges      []RedfishInputRange `json:"InputRanges,omitempty"`
}

type RedfishVoltage struct {
	Name                      string        `json:"Name"`
	ReadingVolts              *float64      `json:"ReadingVolts"`
	UpperThresholdNonCritical *float64      `json:"UpperThresholdNonCritical"`
	LowerThresholdNonCritical *float64      `json:"LowerThresholdNonCritical"`
	UpperThresholdCritical    *float64      `json:"UpperThresholdCritical"`
	LowerThresholdCritical    *float64      `json:"LowerThresholdCritical"`
	Status                    RedfishStatus `json:"Status"`
}

type RedfishInputRange struct {
	InputType      string   `json:"InputType"`
	MinimumVoltage *float64 `json:"MinimumVoltage"`
	MaximumVoltage *float64 `json:"MaximumVoltage"`
	OutputWattage  *float64 `json:"OutputWattage"`
}

type RedfishStatus struct {
	State  string `json:"State"`
	Health string `json:"Health"`
}

type RedfishSensorCollection struct {
	Members []RedfishSensorReference `json:"Members"`
}

type RedfishSensorReference struct {
	ID string `json:"@odata.id"`
}

type RedfishSensor struct {
	Name                      string        `json:"Name"`
	Reading                   *float64      `json:"Reading"`
	ReadingUnits              string        `json:"ReadingUnits"`
	ReadingType               string        `json:"ReadingType"`
	UpperThresholdNonCritical *float64      `json:"UpperThresholdNonCritical"`
	LowerThresholdNonCritical *float64      `json:"LowerThresholdNonCritical"`
	UpperThresholdCritical    *float64      `json:"UpperThresholdCritical"`
	LowerThresholdCritical    *float64      `json:"LowerThresholdCritical"`
	Status                    RedfishStatus `json:"Status"`
}

type RedfishChassisCollection struct {
	Members []RedfishChassisReference `json:"Members"`
}

type RedfishChassisReference struct {
	ID string `json:"@odata.id"`
}

// Структуры для параллельной обработки
type RedfishRequest struct {
	URL      string
	Username string
	Password string
}

type RedfishResponse struct {
	Data  []byte
	Error error
	URL   string
}

type ChassisJob struct {
	ChassisRef RedfishChassisReference
	BaseURL    string
	Config     *Config
}

type ChassisResult struct {
	Powers []PowerInfo
	Error  error
	ID     string
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorGray   = "\033[90m"
	ColorCyan   = "\033[96m"
	ColorPurple = "\033[95m"
)

var debugMode bool

func printColored(color, message string) {
	fmt.Printf("%s%s%s\n", color, message, ColorReset)
}

func printSuccess(message string) {
	printColored(ColorGreen, message)
}

func printInfo(message string) {
	printColored(ColorBlue, message)
}

func printDebug(message string) {
	if debugMode {
		printColored(ColorWhite, message)
	}
}

func printWarning(message string) {
	printColored(ColorYellow, message)
}

func printError(message string) {
	printColored(ColorRed, message)
}

func showHelp() {
	fmt.Printf("Power Test %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file (default: power_config.json)")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected power sensors without configuration check")
	fmt.Println("  -vis        Show visual power layout")
	fmt.Println("  -test       Test IPMI connection and show basic info")
	fmt.Println("  -u <user:pass> Set Redfish username and password")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

// === BMC IP DISCOVERY VIA IPMI ===

func getBMCIPAddress() (string, error) {
	channels := []uint8{0x0E, 0x01, 0x02, 0x06, 0x07}
	const ipParam = 0x03

	for _, ch := range channels {
		dev, err := ipmi.Open(0)
		if err != nil {
			continue
		}
		data, err := dev.GetLanConfig(ch, ipParam)
		dev.Close()
		if err != nil || len(data) < 5 {
			continue
		}

		// Собираем IP-адрес из байтов 1..4 в прямом порядке
		ipAddr := net.IPv4(data[2], data[3], data[4], data[5])
		if ipv4 := ipAddr.To4(); ipv4 != nil && !ipv4.IsUnspecified() {
			fmt.Println(ipAddr)
			return ipv4.String(), nil
		}
	}

	return "", fmt.Errorf("BMC IP address not found on any channel")
}

// === IPMI CLIENT MANAGEMENT ===

func createIPMIClient(timeout time.Duration) (*ipmi.IPMI, context.Context, context.CancelFunc, error) {
	printDebug("Opening IPMI device via u-root...")
	dev, err := ipmi.Open(0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open IPMI device: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return dev, ctx, cancel, nil
}

// === PARALLEL REDFISH CLIENT ===

func createRedfishClient(timeout int) *http.Client {
	return &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Параллельный воркер для Redfish запросов
func redfishWorker(client *http.Client, requests <-chan RedfishRequest, responses chan<- RedfishResponse, wg *sync.WaitGroup) {
	defer wg.Done()

	for req := range requests {
		printDebug(fmt.Sprintf("Making parallel Redfish request to: %s", req.URL))

		httpReq, err := http.NewRequest("GET", req.URL, nil)
		if err != nil {
			responses <- RedfishResponse{Error: err, URL: req.URL}
			continue
		}

		if req.Username != "" && req.Password != "" {
			httpReq.SetBasicAuth(req.Username, req.Password)
		}

		httpReq.Header.Set("Accept", "application/json")

		resp, err := client.Do(httpReq)
		if err != nil {
			responses <- RedfishResponse{Error: err, URL: req.URL}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			responses <- RedfishResponse{
				Error: fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status),
				URL:   req.URL,
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		responses <- RedfishResponse{
			Data:  body,
			Error: err,
			URL:   req.URL,
		}
	}
}

func makeParallelRedfishRequests(client *http.Client, requests []RedfishRequest, workers int) map[string]RedfishResponse {
	if workers <= 0 {
		workers = 3 // Значение по умолчанию
	}

	requestChan := make(chan RedfishRequest, len(requests))
	responseChan := make(chan RedfishResponse, len(requests))

	// Запускаем воркеров
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go redfishWorker(client, requestChan, responseChan, &wg)
	}

	// Отправляем запросы
	go func() {
		for _, req := range requests {
			requestChan <- req
		}
		close(requestChan)
	}()

	// Собираем ответы
	go func() {
		wg.Wait()
		close(responseChan)
	}()

	results := make(map[string]RedfishResponse)
	for resp := range responseChan {
		results[resp.URL] = resp
	}

	return results
}

func makeRedfishRequest(client *http.Client, url, username, password string) ([]byte, error) {
	requests := []RedfishRequest{{URL: url, Username: username, Password: password}}
	results := makeParallelRedfishRequests(client, requests, 1)

	if result, ok := results[url]; ok {
		return result.Data, result.Error
	}

	return nil, fmt.Errorf("no response received for %s", url)
}

// === VALUE VALIDATION ===

func isValidReading(value float64, units string) bool {
	// Фильтруем явно неправильные значения
	if value < 0 {
		return false
	}

	unitsUpper := strings.ToUpper(units)

	// Проверки по типу единиц измерения
	switch {
	case strings.Contains(unitsUpper, "V") || strings.Contains(unitsUpper, "VOLT"):
		// Voltage: разумные пределы 0-400V
		return value >= 0 && value <= 400
	case strings.Contains(unitsUpper, "A") || strings.Contains(unitsUpper, "AMP"):
		// Current: разумные пределы 0-1000A
		return value >= 0 && value <= 1000
	case strings.Contains(unitsUpper, "W") || strings.Contains(unitsUpper, "WATT"):
		// Power: разумные пределы 0-10000W
		return value >= 0 && value <= 10000
	}

	// Дополнительная проверка на подозрительные значения
	suspiciousValues := []float64{32784, 32768, 65535, 65536, 4294967295}
	for _, suspicious := range suspiciousValues {
		if value == suspicious {
			return false
		}
	}

	return true
}

// === REDFISH POWER DETECTION WITH PARALLELISM ===

func getPowerInfoRedfish(config *Config) ([]PowerInfo, error) {
	printDebug("Starting parallel Redfish power detection...")

	// Получаем IP адрес BMC через IPMI
	bmcIP, err := getBMCIPAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get BMC IP: %v", err)
	}

	baseURL := fmt.Sprintf("https://%s", bmcIP)
	printInfo(fmt.Sprintf("Using BMC at %s for parallel Redfish API", bmcIP))

	timeout := config.RedfishTimeout
	if timeout == 0 {
		timeout = 30
	}

	workers := config.RedfishWorkers
	if workers == 0 {
		workers = 5 // Значение по умолчанию
	}

	client := createRedfishClient(timeout)

	// Получаем список chassis
	chassisURL := fmt.Sprintf("%s/redfish/v1/Chassis", baseURL)
	chassisData, err := makeRedfishRequest(client, chassisURL, config.RedfishUsername, config.RedfishPassword)
	if err != nil {
		return nil, fmt.Errorf("failed to get chassis list: %v", err)
	}

	var chassisCollection RedfishChassisCollection
	if err := json.Unmarshal(chassisData, &chassisCollection); err != nil {
		return nil, fmt.Errorf("failed to parse chassis collection: %v", err)
	}

	printDebug(fmt.Sprintf("Found %d chassis, processing in parallel with %d workers", len(chassisCollection.Members), workers))

	// Параллельная обработка chassis
	chassisJobs := make(chan ChassisJob, len(chassisCollection.Members))
	chassisResults := make(chan ChassisResult, len(chassisCollection.Members))

	// Запускаем воркеров для обработки chassis
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go chassisWorker(client, chassisJobs, chassisResults, &wg)
	}

	// Отправляем задания
	go func() {
		for _, chassisRef := range chassisCollection.Members {
			chassisJobs <- ChassisJob{
				ChassisRef: chassisRef,
				BaseURL:    baseURL,
				Config:     config,
			}
		}
		close(chassisJobs)
	}()

	// Собираем результаты
	go func() {
		wg.Wait()
		close(chassisResults)
	}()

	var allPowers []PowerInfo
	var errors []string

	for result := range chassisResults {
		if result.Error != nil {
			errors = append(errors, fmt.Sprintf("Chassis %s: %v", result.ID, result.Error))
		} else {
			allPowers = append(allPowers, result.Powers...)
		}
	}

	if len(errors) > 0 {
		for _, errMsg := range errors {
			printWarning(errMsg)
		}
	}

	if len(allPowers) == 0 {
		return nil, fmt.Errorf("no power sensors found via parallel Redfish processing")
	}

	printDebug(fmt.Sprintf("Collected %d power readings via parallel Redfish", len(allPowers)))
	return allPowers, nil
}

// Воркер для параллельной обработки chassis
func chassisWorker(client *http.Client, jobs <-chan ChassisJob, results chan<- ChassisResult, wg *sync.WaitGroup) {
	defer wg.Done()

	for job := range jobs {
		chassisURL := fmt.Sprintf("%s%s", job.BaseURL, job.ChassisRef.ID)
		printDebug(fmt.Sprintf("Processing chassis in parallel: %s", job.ChassisRef.ID))

		chassisData, err := makeRedfishRequest(client, chassisURL, job.Config.RedfishUsername, job.Config.RedfishPassword)
		if err != nil {
			results <- ChassisResult{Error: err, ID: job.ChassisRef.ID}
			continue
		}

		var chassis RedfishChassis
		if err := json.Unmarshal(chassisData, &chassis); err != nil {
			results <- ChassisResult{Error: err, ID: job.ChassisRef.ID}
			continue
		}

		// Получаем информацию о питании
		if chassis.Power.ID != "" {
			powers, err := getPowerFromChassis(client, job.BaseURL, chassis.Power.ID, job.Config)
			results <- ChassisResult{Powers: powers, Error: err, ID: chassis.ID}
		} else {
			results <- ChassisResult{Powers: []PowerInfo{}, Error: nil, ID: chassis.ID}
		}
	}
}

func getPowerFromChassis(client *http.Client, baseURL, powerPath string, config *Config) ([]PowerInfo, error) {
	powerURL := fmt.Sprintf("%s%s", baseURL, powerPath)
	printDebug(fmt.Sprintf("Getting power info from: %s", powerPath))

	powerData, err := makeRedfishRequest(client, powerURL, config.RedfishUsername, config.RedfishPassword)
	if err != nil {
		return nil, err
	}

	var power RedfishPower
	if err := json.Unmarshal(powerData, &power); err != nil {
		return nil, err
	}

	var powers []PowerInfo

	// Обрабатываем блоки питания
	for i, psu := range power.PowerSupplies {
		if psu.PowerInputWatts != nil && isValidReading(*psu.PowerInputWatts, "W") {
			powers = append(powers, PowerInfo{
				Name:         cleanSensorName(psu.Name),
				Position:     normalizeRedfishPowerPosition(psu.Name + "_PIN"),
				PowerType:    "Power",
				Category:     "PSU",
				Value:        *psu.PowerInputWatts,
				Units:        "W",
				Status:       normalizeRedfishStatus(psu.Status),
				SensorNumber: uint8(i + 1),
				RawValue:     *psu.PowerInputWatts,
			})
		}

		if psu.PowerOutputWatts != nil && isValidReading(*psu.PowerOutputWatts, "W") {
			powers = append(powers, PowerInfo{
				Name:         cleanSensorName(psu.Name) + " OUT",
				Position:     normalizeRedfishPowerPosition(psu.Name + "_POUT"),
				PowerType:    "Power",
				Category:     "PSU",
				Value:        *psu.PowerOutputWatts,
				Units:        "W",
				Status:       normalizeRedfishStatus(psu.Status),
				SensorNumber: uint8(i + 1),
				RawValue:     *psu.PowerOutputWatts,
			})
		}

		if psu.LineInputVoltage != nil && isValidReading(*psu.LineInputVoltage, "V") {
			powers = append(powers, PowerInfo{
				Name:         cleanSensorName(psu.Name) + " VIN",
				Position:     normalizeRedfishPowerPosition(psu.Name + "_VIN"),
				PowerType:    "Voltage",
				Category:     "PSU",
				Value:        *psu.LineInputVoltage,
				Units:        "V",
				Status:       normalizeRedfishStatus(psu.Status),
				SensorNumber: uint8(i + 1),
				RawValue:     *psu.LineInputVoltage,
			})
		}
	}

	// Обрабатываем напряжения
	for i, voltage := range power.Voltages {
		if voltage.ReadingVolts != nil && isValidReading(*voltage.ReadingVolts, "V") {
			minV := 0.0
			maxV := 0.0
			cMin := 0.0
			cMax := 0.0

			if voltage.LowerThresholdNonCritical != nil && *voltage.LowerThresholdNonCritical > 0 {
				minV = *voltage.LowerThresholdNonCritical
			}
			if voltage.UpperThresholdNonCritical != nil && *voltage.UpperThresholdNonCritical > 0 {
				maxV = *voltage.UpperThresholdNonCritical
			}
			if voltage.LowerThresholdCritical != nil && *voltage.LowerThresholdCritical > 0 {
				cMin = *voltage.LowerThresholdCritical
			}
			if voltage.UpperThresholdCritical != nil && *voltage.UpperThresholdCritical > 0 {
				cMax = *voltage.UpperThresholdCritical
			}

			powers = append(powers, PowerInfo{
				Name:         cleanSensorName(voltage.Name),
				Position:     normalizeRedfishPowerPosition(voltage.Name),
				PowerType:    "Voltage",
				Category:     determinePowerCategory(voltage.Name),
				Value:        *voltage.ReadingVolts,
				Units:        "V",
				MinValue:     minV,
				MaxValue:     maxV,
				CriticalMin:  cMin,
				CriticalMax:  cMax,
				Status:       normalizeRedfishStatus(voltage.Status),
				SensorNumber: uint8(i + 100),
				RawValue:     *voltage.ReadingVolts,
			})
		}
	}

	return powers, nil
}

// === HELPER FUNCTIONS FOR REDFISH ===

func cleanSensorName(name string) string {
	// Очищаем имя от лишних символов и приводим к читаемому виду
	name = strings.TrimSpace(name)

	// Убираем повторяющиеся пробелы
	name = regexp.MustCompile(`\s+`).ReplaceAllString(name, " ")

	// Убираем нежелательные символы, оставляем буквы, цифры, пробелы, точки, дефисы, подчеркивания
	name = regexp.MustCompile(`[^\w\s\.\-]`).ReplaceAllString(name, "")

	return name
}

func normalizeRedfishStatus(status RedfishStatus) string {
	state := strings.ToUpper(strings.TrimSpace(status.State))
	health := strings.ToUpper(strings.TrimSpace(status.Health))

	// Комбинированная оценка статуса
	if (state == "ENABLED" || state == "ONLINE") && health == "OK" {
		return "OK"
	}
	if health == "WARNING" || health == "DEGRADED" {
		return "WARNING"
	}
	if health == "CRITICAL" || state == "ABSENT" || state == "OFFLINE" {
		return "CRITICAL"
	}
	if state == "DISABLED" || state == "UNAVAILABLE" {
		return "N/A"
	}

	return "UNKNOWN"
}

func normalizeRedfishPowerPosition(powerName string) string {
	name := strings.ToUpper(strings.TrimSpace(powerName))
	name = strings.ReplaceAll(name, " ", "_")

	// Убираем общие префиксы
	prefixes := []string{"VOLT_", "CUR_", "PWR_", "STS_", "SENSOR_"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
			break
		}
	}

	// Паттерны для нормализации позиций
	patterns := []struct {
		regex  *regexp.Regexp
		format string
	}{
		// PSU patterns
		{regexp.MustCompile(`^PSU[\s_]*(\d+)[\s_]*VIN$`), "PSU%s_VIN"},
		{regexp.MustCompile(`^PSU[\s_]*(\d+)[\s_]*IOUT$`), "PSU%s_IOUT"},
		{regexp.MustCompile(`^PSU[\s_]*(\d+)[\s_]*PIN$`), "PSU%s_PIN"},
		{regexp.MustCompile(`^PSU[\s_]*(\d+)[\s_]*POUT$`), "PSU%s_POUT"},
		{regexp.MustCompile(`^PSU[\s_]*(\d+)$`), "PSU%s"},
		{regexp.MustCompile(`^POWER[\s_]*SUPPLY[\s_]*(\d+)$`), "PSU%s"},

		// CPU patterns
		{regexp.MustCompile(`^CPU[\s_]*(\d+)[\s_]*VCCIN$`), "CPU%s_VCCIN"},
		{regexp.MustCompile(`^CPU[\s_]*(\d+)[\s_]*VOLT$`), "CPU%s_VOLT"},
		{regexp.MustCompile(`^CPU[\s_]*(\d+)$`), "CPU%s"},

		// Memory patterns
		{regexp.MustCompile(`^VDDQ[\s_]*([A-Z]+)$`), "VDDQ_%s"},
		{regexp.MustCompile(`^DDR[\s_]*(\d+)$`), "DDR%s"},
		{regexp.MustCompile(`^DIMM[\s_]*([A-Z]\d+)$`), "DIMM_%s"},

		// System rails - улучшенные паттерны
		{regexp.MustCompile(`^(\d+(?:\.\d+)?)V[\s_]*SB$`), "%sVSB"},
		{regexp.MustCompile(`^(\d+(?:\.\d+)?)[\s_]*V[\s_]*STANDBY$`), "%sVSB"},
		{regexp.MustCompile(`^(\d+(?:\.\d+)?)V$`), "%sV"},
		{regexp.MustCompile(`^(\d+(?:\.\d+)?)[\s_]*VOLT$`), "%sV"},

		// Chipset - улучшенные имена
		{regexp.MustCompile(`^PCH[\s_]*(\w+)$`), "PCH_%s"},
		{regexp.MustCompile(`^CHIPSET[\s_]*(\w+)$`), "PCH_%s"},
		{regexp.MustCompile(`^PCH[\s_]*CORE$`), "PCH_CORE"},
		{regexp.MustCompile(`^PCH[\s_]*1[\s_]*[\.\s]*[\s_]*8V$`), "PCH_1V8"},

		// Battery
		{regexp.MustCompile(`^BATTERY?$`), "BATTERY"},
		{regexp.MustCompile(`^BAT[\s_]*(\d+)$`), "BAT%s"},
	}

	for _, p := range patterns {
		if matches := p.regex.FindStringSubmatch(name); len(matches) > 1 {
			args := make([]interface{}, len(matches)-1)
			for i := 1; i < len(matches); i++ {
				args[i-1] = matches[i]
			}
			return fmt.Sprintf(p.format, args...)
		}
	}

	// Если ничего не подошло, возвращаем очищенное имя
	return name
}

// === POWER UTILITY FUNCTIONS ===

func determinePowerCategory(name string) string {
	name = strings.ToUpper(name)

	if strings.Contains(name, "PSU") || strings.Contains(name, "POWER_SUPPLY") {
		return "PSU"
	} else if strings.Contains(name, "CPU") || strings.Contains(name, "VCCIN") {
		return "CPU"
	} else if strings.Contains(name, "DDR") || strings.Contains(name, "VDDQ") || strings.Contains(name, "MEMORY") || strings.Contains(name, "DIMM") {
		return "Memory"
	} else if strings.Contains(name, "PCH") || strings.Contains(name, "CHIPSET") {
		return "Chipset"
	} else if strings.Contains(name, "BAT") || strings.Contains(name, "BATTERY") {
		return "Battery"
	} else if strings.Contains(name, "12V") || strings.Contains(name, "5V") ||
		strings.Contains(name, "3.3V") || strings.Contains(name, "1.8V") ||
		strings.Contains(name, "1.2V") || strings.Contains(name, "1.05V") ||
		strings.Contains(name, "VSB") || strings.Contains(name, "STANDBY") {
		return "System"
	} else {
		return "Other"
	}
}

// === CUSTOM ROWS GENERATION ===

func generateCustomRowsByCategory(powers []PowerInfo, positionToSlot map[string]int) CustomRowsConfig {
	// Собираем по категориям список номеров слотов
	slotsByCat := make(map[string][]int)
	for _, p := range powers {
		if slot, ok := positionToSlot[p.Position]; ok {
			slotsByCat[p.Category] = append(slotsByCat[p.Category], slot)
		}
	}

	var rows []RowConfig

	// Для каждой категории сортируем номера и делим на непрерывные сегменты
	for cat, slots := range slotsByCat {
		if len(slots) == 0 {
			continue
		}

		sort.Ints(slots)

		// Проходим по отсортированному списку и ищем runs
		runStart := slots[0]
		prev := slots[0]

		for _, s := range slots[1:] {
			if s == prev+1 {
				// Продолжаем текущий run
				prev = s
				continue
			}
			// Прервался — выгружаем run [runStart-prev]
			if runStart == prev {
				rows = append(rows, RowConfig{
					Name:  cat,
					Slots: fmt.Sprintf("%d-%d", runStart, runStart), // Одиночный слот как диапазон
				})
			} else {
				rows = append(rows, RowConfig{
					Name:  cat,
					Slots: fmt.Sprintf("%d-%d", runStart, prev),
				})
			}
			// Начинаем новый run
			runStart = s
			prev = s
		}

		// Последний run
		if runStart == prev {
			rows = append(rows, RowConfig{
				Name:  cat,
				Slots: fmt.Sprintf("%d-%d", runStart, runStart), // Одиночный слот как диапазон
			})
		} else {
			rows = append(rows, RowConfig{
				Name:  cat,
				Slots: fmt.Sprintf("%d-%d", runStart, prev),
			})
		}
	}

	// Сортируем сами ряды по возрастанию номера первого слота
	sort.Slice(rows, func(i, j int) bool {
		var si, sj int
		fmt.Sscanf(rows[i].Slots, "%d", &si)
		fmt.Sscanf(rows[j].Slots, "%d", &sj)
		return si < sj
	})

	return CustomRowsConfig{
		Enabled: false, // По умолчанию выключено
		Rows:    rows,
	}
}

// Генерация PowerVisual с ASCII блочными символами как в fan_test
func generatePowerVisualByCategory(category string) PowerVisual {
	switch category {
	case "PSU":
		return PowerVisual{
			Symbol:      "███",
			ShortName:   "PSU",
			Description: "Power Supply",
			Color:       "yellow",
		}
	case "CPU":
		return PowerVisual{
			Symbol:      "▓▓▓",
			ShortName:   "CPU",
			Description: "CPU Power",
			Color:       "blue",
		}
	case "Memory":
		return PowerVisual{
			Symbol:      "▒▒▒",
			ShortName:   "MEM",
			Description: "Memory Power",
			Color:       "green",
		}
	case "System":
		return PowerVisual{
			Symbol:      "═══",
			ShortName:   "SYS",
			Description: "System Power",
			Color:       "cyan",
		}
	case "Chipset":
		return PowerVisual{
			Symbol:      "≡≡≡",
			ShortName:   "PCH",
			Description: "Chipset Power",
			Color:       "gray",
		}
	case "Battery":
		return PowerVisual{
			Symbol:      "▬▬▬",
			ShortName:   "BAT",
			Description: "Battery",
			Color:       "red",
		}
	default:
		return PowerVisual{
			Symbol:      "░░░",
			ShortName:   "OTH",
			Description: "Other",
			Color:       "white",
		}
	}
}

func checkPowerAgainstRequirements(powers []PowerInfo, config *Config) PowerCheckResult {
	result := PowerCheckResult{
		Status:      "ok",
		VoltageOK:   true,
		CurrentOK:   true,
		PowerOK:     true,
		ToleranceOK: true,
		StatusOK:    true,
	}

	if len(config.PowerRequirements) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	found := make(map[string]bool)
	for _, power := range powers {
		found[power.Position] = true
	}

	for expectedPos := range config.Visualization.PositionToSlot {
		if !found[expectedPos] {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Required sensor at position %s is missing", expectedPos))
			hasErrors = true
		}
	}

	for _, req := range config.PowerRequirements {
		filtered := filterPowers(powers, req)

		if len(filtered) == 0 {
			result.Issues = append(result.Issues,
				fmt.Sprintf("No sensors found matching requirement: %s", req.Name))
			hasErrors = true
			continue
		}

		for _, power := range filtered {
			singleResult := checkSinglePowerAgainstRequirements(power, config.PowerRequirements)
			if singleResult.Status == "error" {
				hasErrors = true
			} else if singleResult.Status == "warning" {
				hasWarnings = true
			}
			result.Issues = append(result.Issues, singleResult.Issues...)
		}
	}

	if hasErrors {
		result.Status = "error"
	} else if hasWarnings {
		result.Status = "warning"
	}

	return result
}

func checkSinglePowerAgainstRequirements(power PowerInfo, requirements []PowerRequirement) PowerCheckResult {
	result := PowerCheckResult{Status: "ok"}

	hasErrors := false
	hasWarnings := false

	for _, req := range requirements {
		if req.PowerType != "" && power.PowerType != req.PowerType {
			continue
		}
		if req.Category != "" && power.Category != req.Category {
			continue
		}

		positionMatch := false
		if len(req.Positions) == 0 {
			positionMatch = true
		} else {
			for _, pos := range req.Positions {
				if power.Position == pos {
					positionMatch = true
					break
				}
			}
		}
		if !positionMatch {
			continue
		}

		if req.ExpectedStatus != nil {
			if expectedStatus, ok := req.ExpectedStatus[power.Position]; ok {
				if expectedStatus == "OK" && power.Status != "OK" {
					hasErrors = true
				} else {
					hasWarnings = true
				}
			}
		}
	}

	if power.MinValue > 0 && power.Value < power.MinValue {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s below threshold %.3f %s",
				power.Value, power.Units, power.MinValue, power.Units))
		hasWarnings = true
	}

	if power.MaxValue > 0 && power.Value > power.MaxValue {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s above threshold %.3f %s",
				power.Value, power.Units, power.MaxValue, power.Units))
		hasWarnings = true
	}

	if power.CriticalMin > 0 && power.Value < power.CriticalMin {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s below critical threshold %.3f %s",
				power.Value, power.Units, power.CriticalMin, power.Units))
		hasErrors = true
	}

	if power.CriticalMax > 0 && power.Value > power.CriticalMax {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s above critical threshold %.3f %s",
				power.Value, power.Units, power.CriticalMax, power.Units))
		hasErrors = true
	}

	if hasErrors {
		result.Status = "error"
	} else if hasWarnings {
		result.Status = "warning"
	}

	return result
}

func filterPowers(powers []PowerInfo, req PowerRequirement) []PowerInfo {
	var filtered []PowerInfo

	for _, power := range powers {
		if req.PowerType != "" && power.PowerType != req.PowerType {
			continue
		}
		if req.Category != "" && power.Category != req.Category {
			continue
		}

		if len(req.Positions) > 0 {
			found := false
			for _, pos := range req.Positions {
				if power.Position == pos {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		filtered = append(filtered, power)
	}

	return filtered
}

func checkPower(config *Config) error {
	printInfo("Starting parallel power check...")

	powers, err := getPowerInfoRedfish(config)
	if err != nil {
		return fmt.Errorf("failed to get power info: %v", err)
	}

	printInfo(fmt.Sprintf("Found power sensors: %d", len(powers)))

	if len(powers) == 0 {
		printError("No power sensors found")
		return fmt.Errorf("no power sensors found")
	}

	categories := make(map[string][]PowerInfo)
	for _, power := range powers {
		categories[power.Category] = append(categories[power.Category], power)
	}

	for category, categoryPowers := range categories {
		visual := generatePowerVisualByCategory(category)
		fmt.Printf("\n%s%s %s sensors:%s\n", getColorByName(visual.Color), visual.Symbol, category, ColorReset)
		for i, power := range categoryPowers {
			statusColor := ColorGreen
			if power.Status != "OK" {
				statusColor = ColorYellow
			}

			fmt.Printf("  %d. %s (%s) - %s%.3f%s%s [%s%s%s]\n",
				i+1, power.Name, power.Position, ColorGreen, power.Value, power.Units, ColorReset,
				statusColor, power.Status, ColorReset)

			if debugMode {
				fmt.Printf("     Type: %s, Sensor: %d, Raw: %.2f\n", power.PowerType, power.SensorNumber, power.RawValue)
				if power.MinValue > 0 || power.MaxValue > 0 {
					fmt.Printf("     Thresholds: %.3f - %.3f %s\n",
						power.MinValue, power.MaxValue, power.Units)
				}
				if power.CriticalMin > 0 || power.CriticalMax > 0 {
					fmt.Printf("     Critical: %.3f - %.3f %s\n",
						power.CriticalMin, power.CriticalMax, power.Units)
				}
			}
		}
	}

	result := checkPowerAgainstRequirements(powers, config)

	if result.Status == "error" {
		printError("POWER CHECK FAILED")
		for _, issue := range result.Issues {
			printError(fmt.Sprintf("  - %s", issue))
		}
		return fmt.Errorf("power check failed with %d issues", len(result.Issues))
	} else if result.Status == "warning" {
		printWarning("POWER CHECK COMPLETED WITH WARNINGS")
		for _, issue := range result.Issues {
			printWarning(fmt.Sprintf("  - %s", issue))
		}
	} else {
		printSuccess("POWER CHECK PASSED")
	}

	return nil
}

// === CONFIGURATION MANAGEMENT ===

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.IPMITimeout == 0 {
		cfg.IPMITimeout = 30
	}
	if cfg.RedfishTimeout == 0 {
		cfg.RedfishTimeout = 30
	}
	if cfg.RedfishWorkers == 0 {
		cfg.RedfishWorkers = 5
	}

	return &cfg, nil
}

func createDefaultConfig(configPath string, username, password string) error {
	printInfo("Creating configuration file using parallel Redfish data...")

	// Создаем временную конфигурацию для получения данных
	tempConfig := &Config{
		RedfishTimeout:  30,
		RedfishUsername: username,
		RedfishPassword: password,
		RedfishWorkers:  5,
	}

	powers, err := getPowerInfoRedfish(tempConfig)
	if err != nil {
		return fmt.Errorf("failed to scan power sensors: %v", err)
	}

	if len(powers) == 0 {
		return fmt.Errorf("no power sensors found to create configuration")
	}

	// Определяем порядок категорий (как в оригинальном коде)
	categoriesOrder := []string{"System", "CPU", "Memory", "Chipset", "Battery", "PSU", "Other"}

	// Группируем powers по категориям
	powersByCat := make(map[string][]PowerInfo)
	for _, p := range powers {
		powersByCat[p.Category] = append(powersByCat[p.Category], p)
	}

	// Для каждой категории сортируем по Position
	for _, cat := range categoriesOrder {
		slice := powersByCat[cat]
		sort.Slice(slice, func(i, j int) bool {
			return slice[i].Position < slice[j].Position
		})
		powersByCat[cat] = slice
	}

	// Присваиваем логические слоты подряд, по группам
	positionToSlot := make(map[string]int, len(powers))
	slotNum := 1
	for _, cat := range categoriesOrder {
		for _, p := range powersByCat[cat] {
			positionToSlot[p.Position] = slotNum
			slotNum++
		}
	}
	totalSlots := slotNum - 1

	// Создаем визуализацию с правильными custom rows
	vis := VisualizationConfig{
		PositionToSlot:  positionToSlot,
		TotalSlots:      totalSlots,
		SlotWidth:       12,
		CategoryVisuals: make(map[string]PowerVisual, len(categoriesOrder)),
		CustomRows:      generateCustomRowsByCategory(powers, positionToSlot),
	}

	for _, cat := range categoriesOrder {
		vis.CategoryVisuals[cat] = generatePowerVisualByCategory(cat)
	}

	config := Config{
		IPMITimeout:     30,
		RedfishTimeout:  30,
		RedfishUsername: username,
		RedfishPassword: password,
		RedfishWorkers:  5,
		PowerRequirements: []PowerRequirement{
			{
				Name:             "PSU Voltage Check",
				Category:         "PSU",
				PowerType:        "Voltage",
				MinValue:         11.5,
				MaxValue:         12.5,
				TolerancePercent: 5.0,
			},
			{
				Name:             "System Rails",
				Category:         "System",
				PowerType:        "Voltage",
				MinValue:         0.5,
				MaxValue:         15.0,
				TolerancePercent: 10.0,
			},
		},
		Visualization: vis,
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return err
	}

	printSuccess("Configuration file created with parallel Redfish power data")
	printInfo(fmt.Sprintf("Total power sensors: %d", totalSlots))
	printInfo(fmt.Sprintf("Parallel workers configured: %d", config.RedfishWorkers))
	printInfo("Custom row layout generated by category (disabled by default)")
	printInfo("To enable custom rows: set 'visualization.custom_rows.enabled' to true")

	return nil
}

// === IPMI TESTING ===

func testIPMI() error {
	printInfo("Testing IPMI connection via u-root...")

	// Open IPMI device with a 30s timeout
	dev, _, cancel, err := createIPMIClient(30 * time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	defer dev.Close()

	printSuccess("IPMI device opened successfully")

	printInfo("Discovering BMC IP...")
	bmcIP, err := getBMCIPAddress()
	if err != nil {
		printWarning(fmt.Sprintf("Failed to discover BMC IP: %v", err))
	} else {
		printSuccess(fmt.Sprintf("BMC IP address: %s", bmcIP))
	}
	return nil
}

// === VISUALIZATION FUNCTIONS ===

// Правильная функция centerText из fan_test с поддержкой рун
func centerText(text string, width int) string {
	if len(text) == 0 {
		return strings.Repeat(" ", width)
	}

	runes := []rune(text)
	runeLen := len(runes)

	if runeLen > width {
		if width > 0 {
			return string(runes[:width])
		}
		return ""
	}

	if runeLen == width {
		return text
	}

	padding := width - runeLen
	leftPad := padding / 2
	rightPad := padding - leftPad

	return strings.Repeat(" ", leftPad) + text + strings.Repeat(" ", rightPad)
}

func parseSlotRange(rangeStr string) (int, int, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format: %s", rangeStr)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start: %v", err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end: %v", err)
	}
	if start > end {
		return 0, 0, fmt.Errorf("start %d greater than end %d", start, end)
	}
	return start, end, nil
}

func rowPositionLabel(positionToSlot map[string]int, slot int) string {
	for pos, s := range positionToSlot {
		if s == slot {
			if len(pos) > 6 {
				return pos[:6]
			}
			return pos
		}
	}
	return ""
}

func getPowerVisual(power PowerInfo, visuals map[string]PowerVisual) PowerVisual {
	if visual, ok := visuals[power.Category]; ok {
		return visual
	}
	return PowerVisual{Symbol: "░░░", ShortName: "???", Description: "Unknown", Color: "white"}
}

func getColorByName(colorName string) string {
	switch colorName {
	case "red":
		return ColorRed
	case "green":
		return ColorGreen
	case "blue":
		return ColorBlue
	case "yellow":
		return ColorYellow
	case "cyan":
		return ColorCyan
	case "purple":
		return ColorPurple
	case "gray":
		return ColorGray
	case "white":
		return ColorWhite
	default:
		return ColorWhite
	}
}

func visualizeSlots(powers []PowerInfo, config *Config) error {
	printInfo("Power Sensors Layout with ASCII Block Icons:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(powers)
	}

	slotData := make([]PowerInfo, maxSlots+1)
	slotResults := make([]PowerCheckResult, maxSlots+1)
	found := make(map[string]bool)
	for _, p := range powers {
		found[p.Position] = true
		if slot, ok := config.Visualization.PositionToSlot[p.Position]; ok && slot >= 1 && slot <= maxSlots {
			slotData[slot] = p
			slotResults[slot] = checkSinglePowerAgainstRequirements(p, config.PowerRequirements)
		}
	}

	for pos, slot := range config.Visualization.PositionToSlot {
		if slot < 1 || slot > maxSlots {
			continue
		}
		if !found[pos] {
			slotResults[slot] = PowerCheckResult{Status: "missing"}
			slotData[slot] = PowerInfo{Name: fmt.Sprintf("MISSING:%s", pos), Position: pos}
		}
	}

	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		perRow := 8
		for start := 1; start <= maxSlots; start += perRow {
			end := start + perRow - 1
			if end > maxSlots {
				end = maxSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Slots %d-%d", start, end),
				Slots: fmt.Sprintf("%d-%d", start, end),
			})
		}
	}

	printInfo("Legend with ASCII Block Icons:")
	fmt.Printf("  %s███%s Present & OK    ", ColorGreen, ColorReset)
	fmt.Printf("  %s▓▓▓%s Issues    ", ColorYellow, ColorReset)
	fmt.Printf("  %s▒▒▒%s Missing    ", ColorRed, ColorReset)
	fmt.Printf("  %s░░░%s Empty Slot\n\n", ColorGray, ColorReset)

	// Показываем легенду категорий с их ASCII символами
	printInfo("Category Icons:")
	for category, visual := range config.Visualization.CategoryVisuals {
		color := getColorByName(visual.Color)
		fmt.Printf("  %s%s%s %s (%s)    ", color, visual.Symbol, ColorReset, category, visual.Description)
	}
	fmt.Printf("\n\n")

	width := config.Visualization.SlotWidth
	if width < 8 {
		width = 8
	}

	for _, row := range rows {
		start, end, err := parseSlotRange(row.Slots)
		if err != nil {
			printError(fmt.Sprintf("Invalid row range %s: %v", row.Slots, err))
			continue
		}

		if start > maxSlots {
			continue
		}
		if end > maxSlots {
			end = maxSlots
		}

		count := end - start + 1
		printInfo(fmt.Sprintf("%s:", row.Name))

		// Top border
		fmt.Print("┌")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Symbol row - используем ASCII символы как в fan_test
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			if slotData[idx].Name != "" && !strings.HasPrefix(slotData[idx].Name, "MISSING:") {
				visual := getPowerVisual(slotData[idx], config.Visualization.CategoryVisuals)
				color := getColorByName(visual.Color)
				txt := centerText(visual.Symbol, width)
				switch slotResults[idx].Status {
				case "error", "missing":
					fmt.Print(ColorRed + txt + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + txt + ColorReset)
				default:
					fmt.Print(color + txt + ColorReset)
				}
			} else {
				// Пустой слот
				fmt.Print(ColorGray + centerText("░░░", width) + ColorReset)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Short name row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			if slotData[idx].Name != "" && !strings.HasPrefix(slotData[idx].Name, "MISSING:") {
				visual := getPowerVisual(slotData[idx], config.Visualization.CategoryVisuals)
				txt := centerText(visual.ShortName, width)
				switch slotResults[idx].Status {
				case "error", "missing":
					fmt.Print(ColorRed + txt + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + txt + ColorReset)
				default:
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else if strings.HasPrefix(slotData[idx].Name, "MISSING:") {
				fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Value + Units row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			if slotData[idx].Name != "" && !strings.HasPrefix(slotData[idx].Name, "MISSING:") {
				valueStr := fmt.Sprintf("%.1f%s", slotData[idx].Value, slotData[idx].Units)
				txt := centerText(valueStr, width)
				switch slotResults[idx].Status {
				case "error", "missing":
					fmt.Print(ColorRed + txt + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + txt + ColorReset)
				default:
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else if strings.HasPrefix(slotData[idx].Name, "MISSING:") {
				fmt.Print(ColorRed + centerText("?", width) + ColorReset)
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Status row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			statusTxt := strings.ToUpper(slotResults[idx].Status)
			if statusTxt == "" {
				statusTxt = "EMPTY"
			}
			if len(statusTxt) > 4 {
				statusTxt = statusTxt[:4]
			}
			txt := centerText(statusTxt, width)
			switch slotResults[idx].Status {
			case "error", "missing":
				fmt.Print(ColorRed + txt + ColorReset)
			case "warning":
				fmt.Print(ColorYellow + txt + ColorReset)
			case "":
				fmt.Print(ColorGray + txt + ColorReset)
			default:
				fmt.Print(ColorGreen + txt + ColorReset)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Bottom border
		fmt.Print("└")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Slot numbers
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			fmt.Print(centerText(fmt.Sprintf("%d", start+i), width+1))
		}
		fmt.Println(" (Slot)")

		// Positions
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			pos := rowPositionLabel(config.Visualization.PositionToSlot, start+i)
			fmt.Print(centerText(pos, width+1))
		}
		fmt.Print(" (Position)\n\n")
	}

	printInfo("Overall Power Check:")
	checkResult := checkPowerAgainstRequirements(powers, config)

	if checkResult.Status == "error" {
		printError("POWER CHECK FAILED")
		for _, issue := range checkResult.Issues {
			printError(fmt.Sprintf("  - %s", issue))
		}
		return fmt.Errorf("power check failed with %d issues", len(checkResult.Issues))
	} else if checkResult.Status == "warning" {
		printWarning("POWER CHECK COMPLETED WITH WARNINGS")
		for _, issue := range checkResult.Issues {
			printWarning(fmt.Sprintf("  - %s", issue))
		}
	} else {
		printSuccess("POWER CHECK PASSED")
	}
	return nil
}

// === USER CREDENTIAL PARSING ===

func parseUserCredentials(userPass string) (string, string, error) {
	if userPass == "" {
		return "", "", nil
	}

	parts := strings.SplitN(userPass, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid format, expected user:password")
	}

	username := strings.TrimSpace(parts[0])
	password := strings.TrimSpace(parts[1])

	if username == "" {
		return "", "", fmt.Errorf("username cannot be empty")
	}

	return username, password, nil
}

// === MAIN FUNCTION ===

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "power_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		listSensors  = flag.Bool("l", false, "List detected power sensors")
		visualize    = flag.Bool("vis", false, "Show visual power layout")
		testMode     = flag.Bool("test", false, "Test IPMI connection")
		userPass     = flag.String("u", "", "Redfish credentials in format user:password")
		debug        = flag.Bool("d", false, "Show debug information")
		help         = flag.Bool("h", false, "Show help")
	)

	flag.Parse()

	debugMode = *debug

	if *help {
		showHelp()
		return
	}

	if *showVersion {
		fmt.Println(VERSION)
		return
	}

	// Парсим учетные данные пользователя
	username, password, err := parseUserCredentials(*userPass)
	if err != nil {
		printError(fmt.Sprintf("Invalid user credentials: %v", err))
		printInfo("Use format: -u username:password")
		os.Exit(1)
	}

	if *testMode {
		err := testIPMI()
		if err != nil {
			printError(fmt.Sprintf("IPMI test failed: %v", err))
			os.Exit(1)
		}
		printSuccess("IPMI and parallel Redfish test completed successfully")
		return
	}

	if *listSensors {
		printInfo("Scanning for power sensors via parallel Redfish...")

		// Создаем временную конфигурацию с переданными credentials
		tempConfig := &Config{
			RedfishTimeout:  30,
			RedfishUsername: username,
			RedfishPassword: password,
			RedfishWorkers:  5,
		}

		powers, err := getPowerInfoRedfish(tempConfig)
		if err != nil {
			printError(fmt.Sprintf("Error getting power information via parallel Redfish: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI/Redfish issues")
			printInfo("Use -u username:password if authentication is required")
			os.Exit(1)
		}

		printInfo(fmt.Sprintf("Found %d power sensors:", len(powers)))

		categories := make(map[string][]PowerInfo)
		for _, power := range powers {
			categories[power.Category] = append(categories[power.Category], power)
		}

		for category, categoryPowers := range categories {
			visual := generatePowerVisualByCategory(category)
			color := getColorByName(visual.Color)
			fmt.Printf("\n%s=== %s %s SENSORS ===%s\n", color, visual.Symbol, category, ColorReset)
			for i, power := range categoryPowers {
				statusColor := ColorGreen
				if power.Status != "OK" {
					statusColor = ColorYellow
				}

				fmt.Printf("  %d. %s (%s) - %s%.3f%s%s [%s%s%s]\n",
					i+1, power.Name, power.Position, ColorGreen, power.Value, power.Units, ColorReset,
					statusColor, power.Status, ColorReset)

				if debugMode {
					fmt.Printf("     Type: %s, Sensor: %d, Raw: %.2f\n", power.PowerType, power.SensorNumber, power.RawValue)
					if power.MinValue > 0 || power.MaxValue > 0 {
						fmt.Printf("     Thresholds: %.3f - %.3f %s\n",
							power.MinValue, power.MaxValue, power.Units)
					}
					if power.CriticalMin > 0 || power.CriticalMax > 0 {
						fmt.Printf("     Critical: %.3f - %.3f %s\n",
							power.CriticalMin, power.CriticalMax, power.Units)
					}
				}
			}
		}
		return
	}

	if *visualize {
		printInfo("Scanning for power sensors via parallel Redfish...")

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		// Обновляем credentials если переданы через -u
		if username != "" {
			config.RedfishUsername = username
			config.RedfishPassword = password
		}

		powers, err := getPowerInfoRedfish(config)
		if err != nil {
			printError(fmt.Sprintf("Error getting power information via parallel Redfish: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI/Redfish issues")
			printInfo("Use -u username:password if authentication is required")
			os.Exit(1)
		}

		err = visualizeSlots(powers, config)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if *createConfig {
		printInfo(fmt.Sprintf("Creating configuration file: %s", *configPath))
		err := createDefaultConfig(*configPath, username, password)
		if err != nil {
			printError(fmt.Sprintf("Error creating configuration: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI/Redfish issues")
			printInfo("Use -u username:password if authentication is required")
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully using parallel Redfish data")
		return
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found power sensors")
		printInfo("Use -test to diagnose IPMI/Redfish connectivity issues")
		os.Exit(1)
	}

	// Обновляем credentials если переданы через -u
	if username != "" {
		config.RedfishUsername = username
		config.RedfishPassword = password
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))
	printInfo(fmt.Sprintf("Using %d parallel workers for Redfish requests", config.RedfishWorkers))

	err = checkPower(config)
	if err != nil {
		printError(fmt.Sprintf("Power check failed: %v", err))
		printInfo("Try running with -test flag to diagnose IPMI/Redfish issues")
		printInfo("Use -u username:password if authentication is required")
		os.Exit(1)
	}
}
