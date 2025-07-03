package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bougou/go-ipmi"
)

const VERSION = "1.0.0"

type PowerInfo struct {
	Name         string  `json:"name"`
	Position     string  `json:"position"`     // PSU1, CPU1, 12V, 5V, VDDQ_A, etc.
	PowerType    string  `json:"power_type"`   // Voltage, Current, Power
	Category     string  `json:"category"`     // PSU, CPU, Memory, System, Chipset, Battery, Other
	Value        float64 `json:"value"`        // текущее значение
	Units        string  `json:"units"`        // Volts, Amps, Watts
	MinValue     float64 `json:"min_value"`    // нижний порог (LNC)
	MaxValue     float64 `json:"max_value"`    // верхний порог (UNC)
	CriticalMin  float64 `json:"critical_min"` // критический нижний порог (LCR)
	CriticalMax  float64 `json:"critical_max"` // критический верхний порог (UCR)
	Status       string  `json:"status"`       // OK, WARNING, CRITICAL, N/A, etc.
	SensorNumber uint8   `json:"sensor_number"`
	RawValue     float64 `json:"raw_value"` // сырое значение из IPMI
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
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type RowConfig struct {
	Name  string `json:"name"`  // Отображаемое имя ряда
	Slots string `json:"slots"` // Диапазон слотов, например "1-8", "9-16"
}

// Настройка кастомных рядов
type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Включить кастомные ряды
	Rows    []RowConfig `json:"rows"`    // Сами ряды
}

// Расширяем существующий VisualizationConfig:
type VisualizationConfig struct {
	CategoryVisuals map[string]PowerVisual `json:"category_visuals"`
	PositionToSlot  map[string]int         `json:"position_to_slot"`
	TotalSlots      int                    `json:"total_slots"`
	SlotWidth       int                    `json:"slot_width"`

	// Новое поле для кастомных рядов
	CustomRows CustomRowsConfig `json:"custom_rows"`
}

type Config struct {
	PowerRequirements []PowerRequirement  `json:"power_requirements"`
	Visualization     VisualizationConfig `json:"visualization"`
	IPMITimeout       int                 `json:"ipmi_timeout_seconds"` // таймаут для IPMI операций
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

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorGray   = "\033[90m"
)

var (
	powerSDRIndices []int
	powerSDROnce    sync.Once
	powerSDRMutex   sync.Mutex
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
	fmt.Printf("Version %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file (default: power_config.json)")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected power sensors without configuration check")
	fmt.Println("  -vis        Show visual power layout")
	fmt.Println("  -test       Test IPMI connection and show basic info")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

// === IPMI CLIENT MANAGEMENT ===

func createIPMIClient() (*ipmi.Client, context.Context, context.CancelFunc, error) {
	client, err := ipmi.NewOpenClient()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create IPMI client: %v", err)
	}

	timeout := 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	if err := client.Connect(ctx); err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("failed to connect to BMC: %v", err)
	}

	printDebug("Successfully connected to BMC via IPMI")
	return client, ctx, cancel, nil
}

// === POWER DETECTION USING IPMI ===

func isPowerSensor(sdr *ipmi.SDR) bool {
	name := strings.ToUpper(sdr.SensorName())

	// исключаем термальные
	for _, kw := range []string{"TEMP", "THERMAL", "°C", "°F"} {
		if strings.Contains(name, kw) {
			printDebug(fmt.Sprintf("Excluding temperature sensor: %s", name))
			return false
		}
	}
	// исключаем всё, что не power (FAN, RPM, PRESENCE, RAID и т.п.)
	for _, kw := range []string{"FAN", "RPM", "SPEED", "PRES", "PRESENCE", "EVENT", "DISCRETE", "HUMIDITY", "RAID"} {
		if strings.Contains(name, kw) {
			printDebug(fmt.Sprintf("Excluding non-power sensor: %s", name))
			return false
		}
	}

	if sdr.Full != nil {
		switch sdr.Full.SensorType {
		case ipmi.SensorTypeVoltage,
			ipmi.SensorTypeCurrent,
			ipmi.SensorTypePowerSupply,
			ipmi.SensorTypePowerUnit: // <-- новый тип
			return true
		case ipmi.SensorTypeTemperature,
			ipmi.SensorTypeFan:
			return false
		}
	}

	// дополнительно по ключевым словам
	for _, kw := range []string{
		"VOLT", "VOUT", "VIN", "VCCIN", "VDDQ", "BAT", "12V", "5V", "3.3V",
		"1.8V", "1.2V", "1.05V", "VSB", "CUR", "IOUT", "IIN", "AMPS",
		"CURRENT", "PWR", "POUT", "WATTS", "POWER", "PSU",
	} {
		if strings.Contains(name, kw) {
			return true
		}
	}

	return false
}

func getPowerInfo(timeoutSec int) ([]PowerInfo, error) {
	printDebug("Starting IPMI power detection via SDR…")

	client, rootCtx, cancelRoot, err := createIPMIClient()
	if err != nil {
		return nil, err
	}
	defer cancelRoot()
	defer client.Close(rootCtx)

	// таймаут на получение SDR
	sdrCtx, cancelSDR := context.WithTimeout(rootCtx, time.Duration(timeoutSec)*time.Second)
	defer cancelSDR()

	sdrs, err := client.GetSDRs(sdrCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to get SDRs in %d sec: %v", timeoutSec, err)
	}
	printDebug(fmt.Sprintf("Retrieved %d SDR records", len(sdrs)))

	// Собираем все power-индексы
	var indices []int
	for i, sdr := range sdrs {
		if isPowerSensor(sdr) {
			indices = append(indices, i)
			printDebug(fmt.Sprintf("Detected power sensor: %s (index %d)", sdr.SensorName(), i))
		}
	}
	printDebug(fmt.Sprintf("Total power sensors: %d", len(indices)))

	if len(indices) == 0 {
		return nil, fmt.Errorf("no power sensors found via SDR")
	}

	var powers []PowerInfo
	for _, idx := range indices {
		if idx < 0 || idx >= len(sdrs) {
			continue
		}
		sdr := sdrs[idx]
		name := sdr.SensorName()
		num := uint8(sdr.SensorNumber())

		units := determinePowerUnits(sdr)
		if units == "Unknown" {
			printDebug(fmt.Sprintf("Sensor %s: units unknown, defaulting to Watts", name))
			units = "Watts"
		}

		// чтение значения
		var raw uint8
		var status string
		readingCtx, cancel := context.WithTimeout(rootCtx, 3*time.Second)
		reading, err := client.GetSensorReading(readingCtx, num)
		cancel()
		if err == nil && reading != nil {
			raw = reading.Reading
			status = normalizeStatus(reading.ActiveStates)
			printDebug(fmt.Sprintf("Reading %s: raw=%d", name, raw))
		} else {
			printDebug(fmt.Sprintf("GetSensorReading failed for %s: %v", name, err))
		}

		// конвертация и пороги из SDR.Full
		if sdr.Full == nil {
			printDebug(fmt.Sprintf("Skipping %s: no Full SDR data", name))
			continue
		}
		value := sdr.Full.ConvertReading(raw)
		minV := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LNC_Raw))
		maxV := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UNC_Raw))
		cmin := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LCR_Raw))
		cmax := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UCR_Raw))

		// диапазон здравого смысла
		switch units {
		case "Volts":
			if value < 0 || value > 50 {
				continue
			}
		case "Amps":
			if value < 0 || value > 100 {
				continue
			}
		case "Watts":
			if value < 0 || value > 5000 {
				continue
			}
		}

		powers = append(powers, PowerInfo{
			Name:         name,
			Position:     normalizeIPMIPowerPosition(name),
			PowerType:    determinePowerType(units),
			Category:     determinePowerCategory(name),
			Value:        value,
			Units:        units,
			MinValue:     minV,
			MaxValue:     maxV,
			CriticalMin:  cmin,
			CriticalMax:  cmax,
			Status:       status,
			SensorNumber: num,
			RawValue:     float64(raw),
		})
	}

	if len(powers) == 0 {
		return nil, fmt.Errorf("no valid power sensors after filtering")
	}
	printDebug(fmt.Sprintf("Collected %d power readings", len(powers)))
	return powers, nil
}

func determinePowerUnits(sdr *ipmi.SDR) string {
	if sdr.Full == nil {
		return "Unknown"
	}

	// Сначала проверяем по типу сенсора IPMI
	switch sdr.Full.SensorType {
	case ipmi.SensorTypeVoltage:
		return "V"
	case ipmi.SensorTypeCurrent:
		return "A"
	case ipmi.SensorTypePowerSupply:
		return "W"
	}

	// Если тип не определен, проверяем по имени
	name := strings.ToUpper(sdr.SensorName())

	// Voltage patterns
	voltageKeywords := []string{"VOLT", "VIN", "VOUT", "VCCIN", "VDDQ", "BAT", "PCH", "12V", "5V", "3.3V", "1.8V", "1.2V", "1.05V", "VSB"}
	for _, keyword := range voltageKeywords {
		if strings.Contains(name, keyword) {
			return "V"
		}
	}

	// Current patterns
	currentKeywords := []string{"CUR", "IOUT", "IIN", "AMPS", "CURRENT"}
	for _, keyword := range currentKeywords {
		if strings.Contains(name, keyword) {
			return "A"
		}
	}

	// Power patterns
	powerKeywords := []string{"PWR", "PIN", "POUT", "WATTS", "POWER"}
	for _, keyword := range powerKeywords {
		if strings.Contains(name, keyword) {
			return "W"
		}
	}

	return "Unknown"
}

func determinePowerType(units string) string {
	switch units {
	case "Volts":
		return "Voltage"
	case "Amps":
		return "Current"
	case "Watts":
		return "Power"
	default:
		return "Unknown"
	}
}

func determinePowerCategory(name string) string {
	name = strings.ToUpper(name)

	if strings.Contains(name, "PSU") || strings.Contains(name, "POWER") {
		return "PSU"
	} else if strings.Contains(name, "CPU") || strings.Contains(name, "VCCIN") {
		return "CPU"
	} else if strings.Contains(name, "DDR") || strings.Contains(name, "VDDQ") || strings.Contains(name, "MEMORY") {
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

func normalizeIPMIPowerPosition(powerName string) string {
	// 1) Uppercase + поменяем пробелы на подчёрки
	name := strings.ToUpper(powerName)
	name = strings.ReplaceAll(name, " ", "_")

	// 2) Сбрасываем распространённые префиксы, чтобы не тащить "VOLT_", "CUR_" и т.п.
	for _, prefix := range []string{"VOLT_", "CUR_", "PWR_", "STS_"} {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
			break
		}
	}

	// 3) Набор regexp-паттернов для разбора разных позиций
	patterns := []struct {
		regex  *regexp.Regexp
		format string
	}{
		// Блок питания
		{regexp.MustCompile(`^PSU(\d+)_VIN$`), "PSU%s_VIN"},
		{regexp.MustCompile(`^PSU(\d+)_IOUT$`), "PSU%s_IOUT"},
		{regexp.MustCompile(`^PSU(\d+)_PIN$`), "PSU%s_PIN"},
		{regexp.MustCompile(`^PSU(\d+)_POUT$`), "PSU%s_POUT"},
		{regexp.MustCompile(`^PSU(\d+)$`), "PSU%s"},

		// CPU rails
		{regexp.MustCompile(`^CPU(\d+)_?VCCIN$`), "CPU%s"},
		{regexp.MustCompile(`^CPU(\d+)$`), "CPU%s"},

		// Память: VDDQ_ABCD, VDDQ_EFGH и т.п.
		{regexp.MustCompile(`^VDDQ_?([A-Z]+)$`), "VDDQ_%s"},

		// Другие шинные названия
		{regexp.MustCompile(`^DDR(\d+)$`), "DDR%s"},
		{regexp.MustCompile(`^PCH_(\w+)$`), "PCH_%s"},
		{regexp.MustCompile(`^BATTERY?$`), "BAT"},
		{regexp.MustCompile(`^3\.3VSB$`), "3.3VSB"},
		{regexp.MustCompile(`^5VSB$`), "5VSB"},

		// Любой вид «число(.число)V»
		{regexp.MustCompile(`^(\d+(?:\.\d+)?)V$`), "%sV"},
	}

	for _, p := range patterns {
		if m := p.regex.FindStringSubmatch(name); len(m) > 1 {
			// Соберём аргументы для fmt.Sprintf
			args := make([]interface{}, len(m)-1)
			for i := 1; i < len(m); i++ {
				args[i-1] = m[i]
			}
			return fmt.Sprintf(p.format, args...)
		}
	}

	// 4) Если ни один паттерн не сработал — просто возвращаем очищённое имя
	return name
}

func normalizeStatus(status interface{}) string {
	var statusStr string
	switch s := status.(type) {
	case string:
		statusStr = s
	case fmt.Stringer:
		statusStr = s.String()
	default:
		statusStr = fmt.Sprintf("%v", status)
	}

	statusStr = strings.ToUpper(strings.TrimSpace(statusStr))

	// Маппинг распространенных статусов
	switch statusStr {
	case "OK", "NORMAL", "GOOD":
		return "OK"
	case "WARNING", "WARN", "MINOR":
		return "WARNING"
	case "CRITICAL", "CRIT", "MAJOR", "ALARM":
		return "CRITICAL"
	case "N/A", "NA", "NOT_AVAILABLE", "UNAVAILABLE":
		return "N/A"
	case "UNKNOWN", "UNK":
		return "UNKNOWN"
	default:
		if statusStr == "" {
			return "UNKNOWN"
		}
		return statusStr
	}
}

func getSafeThreshold(threshold float64) float64 {
	// Проверяем на разумные значения для power сенсоров
	if threshold < -100 || threshold > 1000 {
		return 0
	}
	// Исключаем явно температурные значения (обычно >40 для voltage)
	if threshold > 50.0 {
		return 0
	}
	return threshold
}

// === IPMI TESTING FUNCTION ===

func testIPMI() error {
	printInfo("Testing IPMI connection...")

	client, ctx, cancel, err := createIPMIClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer client.Close(ctx)

	printSuccess("IPMI connection established")

	// Тестируем базовую информацию о системе
	printInfo("Getting device ID...")
	deviceID, err := client.GetDeviceID(ctx)
	if err != nil {
		printWarning(fmt.Sprintf("Failed to get device ID: %v", err))
	} else {
		printSuccess(fmt.Sprintf("Device ID: %02X", deviceID.DeviceID))
		printInfo(fmt.Sprintf("  Manufacturer ID: %06X", deviceID.ManufacturerID))
		printInfo(fmt.Sprintf("  Product ID: %04X", deviceID.ProductID))
		printInfo(fmt.Sprintf("  Firmware Version: %s", deviceID.FirmwareVersionStr()))
	}

	// Тестируем доступ к SDR
	printInfo("Testing SDR access...")
	sdrs, err := client.GetSDRs(ctx)
	if err != nil {
		printError(fmt.Sprintf("SDR access failed: %v", err))
		return fmt.Errorf("SDR access required for power sensor detection")
	} else {
		printSuccess(fmt.Sprintf("SDR access OK: found %d records", len(sdrs)))

		// Показываем статистику по типам сенсоров
		voltageCount := 0
		currentCount := 0
		powerCount := 0

		for _, sdr := range sdrs {
			if isPowerSensor(sdr) {
				units := determinePowerUnits(sdr)
				switch units {
				case "Volts":
					voltageCount++
				case "Amps":
					currentCount++
				case "Watts":
					powerCount++
				}
			}
		}
		printInfo(fmt.Sprintf("  Voltage sensors found: %d", voltageCount))
		printInfo(fmt.Sprintf("  Current sensors found: %d", currentCount))
		printInfo(fmt.Sprintf("  Power sensors found: %d", powerCount))
	}

	return nil
}

// === CONFIGURATION AND CHECKING ===

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Set defaults
	if cfg.IPMITimeout == 0 {
		cfg.IPMITimeout = 30
	}

	return &cfg, nil
}

// Исправленная функция проверки требований
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

	for _, req := range config.PowerRequirements {
		matchingPowers := filterPowers(powers, req)

		// Проверяем позиции, если они указаны
		if len(req.Positions) > 0 {
			for _, expectedPos := range req.Positions {
				found := false
				for _, power := range matchingPowers {
					if power.Position == expectedPos {
						found = true

						// Проверяем пороговые значения
						if req.MinValue > 0 && power.Value < req.MinValue {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s (min required %.3f %s)",
									power.Name, power.Value, power.Units, req.MinValue, power.Units))
							if power.PowerType == "Voltage" {
								result.VoltageOK = false
							} else if power.PowerType == "Current" {
								result.CurrentOK = false
							} else if power.PowerType == "Power" {
								result.PowerOK = false
							}
							hasErrors = true
						}

						if req.MaxValue > 0 && power.Value > req.MaxValue {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s (max allowed %.3f %s)",
									power.Name, power.Value, power.Units, req.MaxValue, power.Units))
							if power.PowerType == "Voltage" {
								result.VoltageOK = false
							} else if power.PowerType == "Current" {
								result.CurrentOK = false
							} else if power.PowerType == "Power" {
								result.PowerOK = false
							}
							hasErrors = true
						}

						// Проверяем пороги из SDR
						if power.MinValue > 0 && power.Value < power.MinValue {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s below threshold %.3f %s",
									power.Name, power.Value, power.Units, power.MinValue, power.Units))
							hasWarnings = true
							result.ToleranceOK = false
						}

						if power.MaxValue > 0 && power.Value > power.MaxValue {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s above threshold %.3f %s",
									power.Name, power.Value, power.Units, power.MaxValue, power.Units))
							hasWarnings = true
							result.ToleranceOK = false
						}

						// Проверяем критические пороги
						if power.CriticalMin > 0 && power.Value < power.CriticalMin {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s below critical threshold %.3f %s",
									power.Name, power.Value, power.Units, power.CriticalMin, power.Units))
							hasErrors = true
							result.ToleranceOK = false
						}

						if power.CriticalMax > 0 && power.Value > power.CriticalMax {
							result.Issues = append(result.Issues,
								fmt.Sprintf("%s: %.3f %s above critical threshold %.3f %s",
									power.Name, power.Value, power.Units, power.CriticalMax, power.Units))
							hasErrors = true
							result.ToleranceOK = false
						}

						// Проверяем статус
						if expectedStatus, exists := req.ExpectedStatus[expectedPos]; exists {
							if power.Status != expectedStatus {
								result.Issues = append(result.Issues,
									fmt.Sprintf("%s: status %s (expected %s)", power.Name, power.Status, expectedStatus))
								if expectedStatus == "OK" && power.Status != "OK" {
									hasErrors = true
									result.StatusOK = false
								} else {
									hasWarnings = true
									result.StatusWarn = true
								}
							}
						}
						break
					}
				}

				if !found {
					result.Issues = append(result.Issues,
						fmt.Sprintf("Required position %s not found for %s", expectedPos, req.Name))
					hasErrors = true
				}
			}
		} else {
			// Если позиции не указаны, проверяем все найденные
			for _, power := range matchingPowers {
				// Те же проверки, что и выше
				if req.MinValue > 0 && power.Value < req.MinValue {
					result.Issues = append(result.Issues,
						fmt.Sprintf("%s: %.3f %s (min required %.3f %s)",
							power.Name, power.Value, power.Units, req.MinValue, power.Units))
					hasErrors = true
				}

				if req.MaxValue > 0 && power.Value > req.MaxValue {
					result.Issues = append(result.Issues,
						fmt.Sprintf("%s: %.3f %s (max allowed %.3f %s)",
							power.Name, power.Value, power.Units, req.MaxValue, power.Units))
					hasErrors = true
				}
			}
		}
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
		// Filter by type
		if req.PowerType != "" && power.PowerType != req.PowerType {
			continue
		}

		// Filter by category
		if req.Category != "" && power.Category != req.Category {
			continue
		}

		// Filter by positions
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
	printInfo("Starting power check...")

	powers, err := getPowerInfo(config.IPMITimeout)
	if err != nil {
		return fmt.Errorf("failed to get power info: %v", err)
	}

	printInfo(fmt.Sprintf("Found power sensors: %d", len(powers)))

	if len(powers) == 0 {
		printError("No power sensors found")
		return fmt.Errorf("no power sensors found")
	}

	// Display found power sensors by category
	categories := make(map[string][]PowerInfo)
	for _, power := range powers {
		categories[power.Category] = append(categories[power.Category], power)
	}

	for category, categoryPowers := range categories {
		printInfo(fmt.Sprintf("\n%s sensors:", category))
		for i, power := range categoryPowers {
			statusColor := ColorGreen
			if power.Status != "OK" {
				statusColor = ColorYellow
			}

			fmt.Printf("  %d. %s (%s): %s%.3f %s%s [%s%s%s]\n",
				i+1, power.Name, power.Position, ColorGreen, power.Value, power.Units, ColorReset,
				statusColor, power.Status, ColorReset)

			printDebug(fmt.Sprintf("    Type: %s, Sensor: %d, Thresholds: %.3f-%.3f %s",
				power.PowerType, power.SensorNumber, power.MinValue, power.MaxValue, power.Units))
		}
	}

	// Check requirements
	allPassed := true
	if len(config.PowerRequirements) > 0 {
		for _, req := range config.PowerRequirements {
			printInfo(fmt.Sprintf("\nChecking requirement: %s", req.Name))

			result := checkPowerAgainstRequirements(powers, config)
			if result.Status == "error" {
				printError("  Requirement FAILED")
				for _, issue := range result.Issues {
					printError(fmt.Sprintf("    - %s", issue))
				}
				allPassed = false
			} else if result.Status == "warning" {
				printWarning("  Requirement passed with warnings")
				for _, issue := range result.Issues {
					printWarning(fmt.Sprintf("    - %s", issue))
				}
			} else {
				printSuccess("  Requirement PASSED")
			}
		}
	}

	if !allPassed {
		printError("Power configuration validation FAILED!")
		return fmt.Errorf("power configuration validation failed")
	} else {
		printSuccess("All power sensors within acceptable ranges!")
		return nil
	}
}

// === VISUALIZATION ===

func generatePowerVisualByCategory(category string) PowerVisual {
	visual := PowerVisual{
		Description: fmt.Sprintf("%s Power", category),
		Color:       "green",
	}

	switch category {
	case "PSU":
		visual.Symbol = "███"
		visual.ShortName = "PSU"
	case "CPU":
		visual.Symbol = "▓▓▓"
		visual.ShortName = "CPU"
	case "Memory":
		visual.Symbol = "═══"
		visual.ShortName = "MEM"
	case "System":
		visual.Symbol = "≡≡≡"
		visual.ShortName = "SYS"
	case "Chipset":
		visual.Symbol = "▒▒▒"
		visual.ShortName = "PCH"
	case "Battery":
		visual.Symbol = "■■■"
		visual.ShortName = "BAT"
	default:
		visual.Symbol = "░░░"
		visual.ShortName = "PWR"
	}

	return visual
}

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

// visualizeSlots рисует табличное представление power-слотов.
func visualizeSlots(powers []PowerInfo, config *Config) error {
	printInfo("Power Sensors Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(powers)
	}

	// Собираем данные по слотам и результаты проверок
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

	// Отметим отсутствующие сенсоры
	for pos, slot := range config.Visualization.PositionToSlot {
		if slot < 1 || slot > maxSlots {
			continue
		}
		if !found[pos] {
			slotResults[slot] = PowerCheckResult{Status: "missing"}
			slotData[slot] = PowerInfo{Name: fmt.Sprintf("MISSING:%s", pos), Position: pos}
		}
	}

	// Собираем ряды: кастомные или legacy по 8
	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		// fallback: один ряд по 8 слотов
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

	// Legend…
	printInfo("Legend:")
	fmt.Printf("  %s▓▓▓%s Present & OK    ", ColorGreen, ColorReset)
	fmt.Printf("  %s▓▓▓%s Issues    ", ColorYellow, ColorReset)
	fmt.Printf("  %s░░░%s Missing    ", ColorRed, ColorReset)
	fmt.Printf("  %s░░░%s Empty Slot\n\n", ColorWhite, ColorReset)

	width := config.Visualization.SlotWidth
	if width < 8 {
		width = 8
	}

	// Рендер каждого ряда
	for _, row := range rows {
		start, end, err := parseSlotRange(row.Slots)
		if err != nil || start < 1 || end > maxSlots {
			printWarning(fmt.Sprintf("Skipping invalid row '%s': %v", row.Slots, err))
			continue
		}
		count := end - start + 1

		// Header
		if len(rows) > 1 {
			fmt.Printf("%s:\n", row.Name)
		}
		// Top border
		fmt.Print("┌")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Символы
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			p := slotData[idx]
			res := slotResults[idx]
			if p.Name != "" {
				vis := generatePowerVisualByCategory(p.Category)
				sym := centerText(vis.Symbol, width)
				switch res.Status {
				case "error", "missing":
					fmt.Print(ColorRed + sym + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + sym + ColorReset)
				default:
					fmt.Print(ColorGreen + sym + ColorReset)
				}
			} else {
				fmt.Print(centerText("░░░", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// ShortName
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			p := slotData[idx]
			res := slotResults[idx]
			if p.Name != "" {
				vis := generatePowerVisualByCategory(p.Category)
				txt := centerText(vis.ShortName, width)
				switch res.Status {
				case "error", "missing":
					fmt.Print(ColorRed + txt + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + txt + ColorReset)
				default:
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Value
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			p := slotData[idx]
			res := slotResults[idx]
			if p.Name != "" && res.Status != "missing" {
				val := fmt.Sprintf("%.1f%s", p.Value, p.Units)
				txt := centerText(val, width)
				switch res.Status {
				case "error":
					fmt.Print(ColorRed + txt + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + txt + ColorReset)
				default:
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Status
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			statusTxt := strings.ToUpper(slotResults[idx].Status)
			txt := centerText(statusTxt, width)
			switch slotResults[idx].Status {
			case "error", "missing":
				fmt.Print(ColorRed + txt + ColorReset)
			case "warning":
				fmt.Print(ColorYellow + txt + ColorReset)
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

	printSuccess("Custom rows layout rendered!")
	return nil
}

// Помощник для извлечения названия позиции по номеру слота
func rowPositionLabel(posMap map[string]int, slotNum int) string {
	for pos, num := range posMap {
		if num == slotNum {
			return pos
		}
	}
	return ""
}

// Функция для проверки одного сенсора против требований
func checkSinglePowerAgainstRequirements(power PowerInfo, requirements []PowerRequirement) PowerCheckResult {
	result := PowerCheckResult{
		Status:      "ok",
		VoltageOK:   true,
		CurrentOK:   true,
		PowerOK:     true,
		ToleranceOK: true,
		StatusOK:    true,
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range requirements {
		// Проверяем соответствие по типу и категории
		if req.PowerType != "" && power.PowerType != req.PowerType {
			continue
		}
		if req.Category != "" && power.Category != req.Category {
			continue
		}

		// Если указаны позиции, проверяем соответствие
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

		// Проверяем пороговые значения из требований
		if req.MinValue > 0 && power.Value < req.MinValue {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%.3f %s below required minimum %.3f %s",
					power.Value, power.Units, req.MinValue, power.Units))
			hasErrors = true
		}

		if req.MaxValue > 0 && power.Value > req.MaxValue {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%.3f %s above required maximum %.3f %s",
					power.Value, power.Units, req.MaxValue, power.Units))
			hasErrors = true
		}

		// Проверяем статус
		if expectedStatus, exists := req.ExpectedStatus[power.Position]; exists {
			if power.Status != expectedStatus {
				result.Issues = append(result.Issues,
					fmt.Sprintf("status %s (expected %s)", power.Status, expectedStatus))
				if expectedStatus == "OK" && power.Status != "OK" {
					hasErrors = true
				} else {
					hasWarnings = true
				}
			}
		}
	}

	// Проверяем пороги из IPMI SDR
	if power.MinValue > 0 && power.Value < power.MinValue {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s below IPMI threshold %.3f %s",
				power.Value, power.Units, power.MinValue, power.Units))
		hasWarnings = true
	}

	if power.MaxValue > 0 && power.Value > power.MaxValue {
		result.Issues = append(result.Issues,
			fmt.Sprintf("%.3f %s above IPMI threshold %.3f %s",
				power.Value, power.Units, power.MaxValue, power.Units))
		hasWarnings = true
	}

	// Проверяем критические пороги
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

func generateCustomRowsByCategory(
	powers []PowerInfo,
	positionToSlot map[string]int,
) CustomRowsConfig {
	// 1) Собираем по категориям список номеров слотов
	slotsByCat := make(map[string][]int)
	for _, p := range powers {
		if slot, ok := positionToSlot[p.Position]; ok {
			slotsByCat[p.Category] = append(slotsByCat[p.Category], slot)
		}
	}

	var rows []RowConfig

	// 2) Для каждой категории сортируем номера и делим на непрерывные сегменты
	for cat, slots := range slotsByCat {
		sort.Ints(slots)
		// проходим по отсортированному списку и ищем runs
		runStart := slots[0]
		prev := slots[0]
		for _, s := range slots[1:] {
			if s == prev+1 {
				// продолжаем текущий run
				prev = s
				continue
			}
			// прервался — выгружаем run [runStart-prev]
			rows = append(rows, RowConfig{
				Name:  cat,
				Slots: fmt.Sprintf("%d-%d", runStart, prev),
			})
			// начинаем новый run
			runStart = s
			prev = s
		}
		// последний run
		rows = append(rows, RowConfig{
			Name:  cat,
			Slots: fmt.Sprintf("%d-%d", runStart, prev),
		})
	}

	// 3) Сортируем сами ряды по возрастанию номера первого слота
	sort.Slice(rows, func(i, j int) bool {
		var si, sj int
		fmt.Sscanf(rows[i].Slots, "%d-", &si)
		fmt.Sscanf(rows[j].Slots, "%d-", &sj)
		return si < sj
	})

	return CustomRowsConfig{
		Enabled: false,
		Rows:    rows,
	}
}

// createDefaultConfig создаёт конфиг и заполняет CustomRowsConfig с учётом категорий
func createDefaultConfig(configPath string) error {
	printInfo("Scanning IPMI for power sensors to create configuration...")

	powers, err := getPowerInfo(30)
	if err != nil {
		return fmt.Errorf("could not scan power sensors via IPMI: %v", err)
	}
	if len(powers) == 0 {
		return fmt.Errorf("no power sensors found - cannot create configuration")
	}

	// 1) Определяем порядок категорий
	categoriesOrder := []string{"System", "CPU", "Memory", "Chipset", "Battery", "PSU", "Other"}

	// 2) Группируем powers по категориям
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

	// 3) Присваиваем логические слоты подряд, по группам
	positionToSlot := make(map[string]int, len(powers))
	slotNum := 1
	for _, cat := range categoriesOrder {
		for _, p := range powersByCat[cat] {
			positionToSlot[p.Position] = slotNum
			slotNum++
		}
	}
	totalSlots := slotNum - 1

	// 4) Собираем дефолтные требования
	defaultReqs := []PowerRequirement{
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
	}

	// 5) Формируем VisualizationConfig
	vis := VisualizationConfig{
		PositionToSlot:  positionToSlot,
		TotalSlots:      totalSlots,
		SlotWidth:       12,
		CategoryVisuals: make(map[string]PowerVisual, len(categoriesOrder)),
		// Кастомные ряды можно сгенерить на всякий случай, но они уже будут по одному на категорию:
		CustomRows: generateCustomRowsByCategory(powers, positionToSlot),
	}
	for _, cat := range categoriesOrder {
		vis.CategoryVisuals[cat] = generatePowerVisualByCategory(cat)
	}

	// 6) Заполняем и сохраняем конфиг
	cfg := Config{
		IPMITimeout:       30,
		PowerRequirements: defaultReqs,
		Visualization:     vis,
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return err
	}

	printSuccess("Configuration file created with grouped logical slots")
	return nil
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

	if *testMode {
		err := testIPMI()
		if err != nil {
			printError(fmt.Sprintf("IPMI test failed: %v", err))
			os.Exit(1)
		}
		printSuccess("IPMI test completed successfully")
		return
	}

	if *listSensors {
		printInfo("Scanning for power sensors via IPMI...")
		powers, err := getPowerInfo(30)
		if err != nil {
			printError(fmt.Sprintf("Error getting power information via IPMI: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}

		printInfo(fmt.Sprintf("Found %d power sensors:", len(powers)))

		// Group by category for better display
		categories := make(map[string][]PowerInfo)
		for _, power := range powers {
			categories[power.Category] = append(categories[power.Category], power)
		}

		for category, categoryPowers := range categories {
			fmt.Printf("\n%s=== %s SENSORS ===%s\n", ColorBlue, category, ColorReset)
			for i, power := range categoryPowers {
				statusColor := ColorGreen
				if power.Status != "OK" {
					statusColor = ColorYellow
				}

				fmt.Printf("  %d. %s (%s) - %s%.3f %s%s [%s%s%s]\n",
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
		printInfo("Scanning for power sensors via IPMI...")
		powers, err := getPowerInfo(30)
		if err != nil {
			printError(fmt.Sprintf("Error getting power information via IPMI: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
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
		err := createDefaultConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error creating configuration: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully using IPMI data")
		return
	}

	// По умолчанию: загружаем конфигурацию и выполняем проверку питания
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found power sensors")
		printInfo("Use -test to diagnose IPMI connectivity issues")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkPower(config)
	if err != nil {
		printError(fmt.Sprintf("Power check failed: %v", err))
		printInfo("Try running with -test flag to diagnose IPMI issues")
		os.Exit(1)
	}
}
