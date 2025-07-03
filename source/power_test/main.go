package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
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

type VisualizationConfig struct {
	CategoryVisuals map[string]PowerVisual `json:"category_visuals"` // category -> visual
	PositionToSlot  map[string]int         `json:"position_to_slot"` // position -> logical slot number
	TotalSlots      int                    `json:"total_slots"`
	SlotWidth       int                    `json:"slot_width"`
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
	fmt.Printf("Power Monitor %s\n", VERSION)
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

	// Исключаем температурные сенсоры
	tempKeywords := []string{"TEMP", "THERMAL", "DEGREE", "°C", "°F"}
	for _, keyword := range tempKeywords {
		if strings.Contains(name, keyword) {
			printDebug(fmt.Sprintf("Excluding temperature sensor: %s", name))
			return false
		}
	}

	// Исключаем другие типы сенсоров
	excludeKeywords := []string{"FAN", "RPM", "SPEED", "INTRUSION", "CHASSIS"}
	for _, keyword := range excludeKeywords {
		if strings.Contains(name, keyword) {
			printDebug(fmt.Sprintf("Excluding non-power sensor: %s", name))
			return false
		}
	}

	// Проверяем по типу сенсора
	if sdr.Full != nil {
		switch sdr.Full.SensorType {
		case ipmi.SensorTypeVoltage:
			return true
		case ipmi.SensorTypeCurrent:
			return true
		case ipmi.SensorTypePowerSupply:
			return true
		case ipmi.SensorTypeTemperature:
			printDebug(fmt.Sprintf("Excluding temperature sensor by type: %s", name))
			return false // Явно исключаем температурные
		case ipmi.SensorTypeFan:
			printDebug(fmt.Sprintf("Excluding fan sensor by type: %s", name))
			return false // Явно исключаем вентиляторы
		}
	}

	// Проверяем по имени
	powerKeywords := []string{
		"VOLT", "VIN", "VOUT", "VCCIN", "VDDQ", "BAT",
		"CUR", "IOUT", "IIN", "AMPS", "CURRENT",
		"PWR", "PIN", "POUT", "WATTS", "POWER",
		"12V", "5V", "3.3V", "1.8V", "1.2V", "1.05V", "VSB",
	}

	for _, keyword := range powerKeywords {
		if strings.Contains(name, keyword) {
			return true
		}
	}

	return false
}

func getPowerInfo(timeoutSec int) ([]PowerInfo, error) {
	printDebug("Starting IPMI power detection via SDR...")

	client, rootCtx, cancelRoot, err := createIPMIClient()
	if err != nil {
		return nil, err
	}
	defer cancelRoot()
	defer client.Close(rootCtx)

	// Set up timeout for SDR fetch
	sdrCtx, cancelSDR := context.WithTimeout(rootCtx, time.Duration(timeoutSec)*time.Second)
	defer cancelSDR()

	sdrs, err := client.GetSDRs(sdrCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to get SDR records within %d seconds: %v", timeoutSec, err)
	}
	printDebug(fmt.Sprintf("Retrieved %d SDR records", len(sdrs)))

	// Cache power SDR indices once
	powerSDROnce.Do(func() {
		powerSDRMutex.Lock()
		defer powerSDRMutex.Unlock()
		for i, sdr := range sdrs {
			if sdr.Full != nil && sdr.HasAnalogReading() && isPowerSensor(sdr) {
				powerSDRIndices = append(powerSDRIndices, i)
				printDebug(fmt.Sprintf("Found power sensor: %s (sensor %d)", sdr.SensorName(), sdr.SensorNumber()))
			}
		}
		printDebug(fmt.Sprintf("Cached %d power SDR indices", len(powerSDRIndices)))
	})

	// Prepare slice for power sensors
	powers := make([]PowerInfo, 0, len(powerSDRIndices))

	// Copy indices under lock
	powerSDRMutex.Lock()
	idxs := append([]int(nil), powerSDRIndices...)
	powerSDRMutex.Unlock()

	// Read each power sensor from SDRs
	for _, idx := range idxs {
		if idx < 0 || idx >= len(sdrs) {
			continue
		}
		sdr := sdrs[idx]
		name := sdr.SensorName()
		num := uint8(sdr.SensorNumber())

		units := determinePowerUnits(sdr)

		// Пропускаем сенсоры с неопределенными единицами
		if units == "Unknown" {
			printDebug(fmt.Sprintf("Skipping sensor %s with unknown units", name))
			continue
		}

		var value float64
		var status string
		var rawValue uint8
		var minVal, maxVal, critMin, critMax float64

		// Сначала пробуем получить актуальное значение через GetSensorReading
		readingCtx, cancelReading := context.WithTimeout(rootCtx, 3*time.Second)
		reading, readingErr := client.GetSensorReading(readingCtx, num)
		cancelReading()

		if readingErr == nil && reading != nil {
			// Успешно получили значение через GetSensorReading
			rawValue = reading.Reading
			status = normalizeStatus(reading.ActiveStates)
			printDebug(fmt.Sprintf("Got reading for %s: raw=%d, status=%v", name, rawValue, reading.ActiveStates))
		} else {
			printDebug(fmt.Sprintf("GetSensorReading failed for %s (sensor %d): %v", name, num, readingErr))
		}

		// Конвертируем значение и получаем пороги из SDR
		if sdr.Full != nil {
			if readingErr == nil {
				value = sdr.Full.ConvertReading(rawValue)
			} else {
				// Fallback: используем значение из SDR
				rawValue = uint8(sdr.Full.SensorValue)
				value = sdr.Full.ConvertReading(rawValue)
				status = normalizeStatus(sdr.Full.SensorStatus)
			}

			// Получаем пороги
			minVal = getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LNC_Raw))
			maxVal = getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UNC_Raw))
			critMin = getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LCR_Raw))
			critMax = getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UCR_Raw))
		} else {
			printDebug(fmt.Sprintf("Skipping sensor %s - no Full SDR data available", name))
			continue
		}

		printDebug(fmt.Sprintf("Sensor %s: raw=%d, converted=%.3f %s, status=%s", name, rawValue, value, units, status))

		// Дополнительная фильтрация по значениям
		if units == "Volts" && (value < 0 || value > 50) {
			printDebug(fmt.Sprintf("Skipping voltage sensor %s with value %.2f (out of range)", name, value))
			continue
		}
		if units == "Amps" && (value < 0 || value > 100) {
			printDebug(fmt.Sprintf("Skipping current sensor %s with value %.2f (out of range)", name, value))
			continue
		}
		if units == "Watts" && (value < 0 || value > 5000) {
			printDebug(fmt.Sprintf("Skipping power sensor %s with value %.2f (out of range)", name, value))
			continue
		}

		powers = append(powers, PowerInfo{
			Name:         name,
			Position:     normalizeIPMIPowerPosition(name),
			PowerType:    determinePowerType(units),
			Category:     determinePowerCategory(name),
			Value:        value,
			Units:        units,
			MinValue:     minVal,
			MaxValue:     maxVal,
			CriticalMin:  critMin,
			CriticalMax:  critMax,
			Status:       status,
			SensorNumber: num,
			RawValue:     float64(rawValue),
		})
	}

	if len(powers) == 0 {
		return nil, fmt.Errorf("no power sensors found via IPMI SDR records")
	}
	printDebug(fmt.Sprintf("Collected %d power sensors via SDR", len(powers)))
	return powers, nil
}

func determinePowerUnits(sdr *ipmi.SDR) string {
	if sdr.Full == nil {
		return "Unknown"
	}

	// Сначала проверяем по типу сенсора IPMI
	switch sdr.Full.SensorType {
	case ipmi.SensorTypeVoltage:
		return "Volts"
	case ipmi.SensorTypeCurrent:
		return "Amps"
	case ipmi.SensorTypePowerSupply:
		return "Watts"
	}

	// Если тип не определен, проверяем по имени
	name := strings.ToUpper(sdr.SensorName())

	// Voltage patterns
	voltageKeywords := []string{"VOLT", "VIN", "VOUT", "VCCIN", "VDDQ", "BAT", "PCH", "12V", "5V", "3.3V", "1.8V", "1.2V", "1.05V", "VSB"}
	for _, keyword := range voltageKeywords {
		if strings.Contains(name, keyword) {
			return "Volts"
		}
	}

	// Current patterns
	currentKeywords := []string{"CUR", "IOUT", "IIN", "AMPS", "CURRENT"}
	for _, keyword := range currentKeywords {
		if strings.Contains(name, keyword) {
			return "Amps"
		}
	}

	// Power patterns
	powerKeywords := []string{"PWR", "PIN", "POUT", "WATTS", "POWER"}
	for _, keyword := range powerKeywords {
		if strings.Contains(name, keyword) {
			return "Watts"
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
	name := strings.ToUpper(powerName)
	name = strings.ReplaceAll(name, " ", "")
	name = strings.ReplaceAll(name, "_", "")

	// Обработка различных паттернов имен IPMI power сенсоров
	patterns := []struct {
		regex  *regexp.Regexp
		format string
	}{
		// PSU patterns
		{regexp.MustCompile(`PSU(\d+)`), "PSU%s"},
		{regexp.MustCompile(`POWERSUPPLY(\d+)`), "PSU%s"},

		// CPU patterns
		{regexp.MustCompile(`CPU(\d+)VCCIN`), "CPU%s"},
		{regexp.MustCompile(`VOLTCPU(\d+)`), "CPU%s"},
		{regexp.MustCompile(`CPU(\d+)`), "CPU%s"},

		// Voltage rail patterns - более специфичные сначала
		{regexp.MustCompile(`VOLT(\d+)\.(\d+)V`), "%s.%sV"},
		{regexp.MustCompile(`(\d+)\.(\d+)V`), "%s.%sV"},
		{regexp.MustCompile(`VOLT(\d+)V(?:SB)?`), "%sV"},
		{regexp.MustCompile(`(\d+)VSB`), "%sVSB"},
		{regexp.MustCompile(`(\d+)V`), "%sV"},

		// Memory patterns
		{regexp.MustCompile(`VOLTVDDQ([A-P]+)`), "VDDQ_%s"},
		{regexp.MustCompile(`VDDQ([A-P]+)`), "VDDQ_%s"},

		// PCH patterns - исправлено для лучшего определения
		{regexp.MustCompile(`VOLTPCH(\w+)`), "PCH_%s"},
		{regexp.MustCompile(`PCH(\w+)`), "PCH_%s"},
		{regexp.MustCompile(`PCH`), "PCH"},

		// Battery
		{regexp.MustCompile(`VOLTBAT`), "BAT"},
		{regexp.MustCompile(`BATTERY`), "BAT"},
		{regexp.MustCompile(`BAT`), "BAT"},

		// Current patterns
		{regexp.MustCompile(`CURPSU(\d+)IOUT`), "PSU%s_CUR"},
		{regexp.MustCompile(`CUR(\w+)IOUT`), "%s_CUR"},

		// Power patterns
		{regexp.MustCompile(`PWRPSU(\d+)P(IN|OUT)`), "PSU%s_PWR"},
		{regexp.MustCompile(`PWR(\w+)P(IN|OUT)`), "%s_PWR"},

		// Generic voltage patterns (fallback)
		{regexp.MustCompile(`VOLT(\w+)`), "%s"},
	}

	for _, pattern := range patterns {
		if matches := pattern.regex.FindStringSubmatch(name); len(matches) > 1 {
			var parts []string
			for _, match := range matches[1:] {
				if match != "" {
					parts = append(parts, match)
				}
			}

			if len(parts) > 0 {
				var result string
				if len(parts) == 1 {
					result = fmt.Sprintf(pattern.format, parts[0])
				} else if len(parts) == 2 {
					result = fmt.Sprintf(pattern.format, parts[0], parts[1])
				} else {
					result = fmt.Sprintf(pattern.format, parts[0])
				}

				// Ограничиваем длину позиции
				if len(result) > 12 {
					result = result[:12]
				}
				return result
			}
		}
	}

	// Fallback: используем исходное имя, но укорачиваем и очищаем
	cleaned := strings.ReplaceAll(name, "VOLT", "")
	cleaned = strings.ReplaceAll(cleaned, "CUR", "")
	cleaned = strings.ReplaceAll(cleaned, "PWR", "")

	if len(cleaned) > 12 {
		cleaned = cleaned[:12]
	}
	if cleaned == "" {
		cleaned = name
		if len(cleaned) > 12 {
			cleaned = cleaned[:12]
		}
	}

	return cleaned
}

func normalizeStatus(status interface{}) string {
	// Обрабатываем различные типы статуса
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

func checkPowerAgainstRequirements(powers []PowerInfo, config *Config) PowerCheckResult {
	result := PowerCheckResult{
		Status:      "ok",
		VoltageOK:   true,
		CurrentOK:   true,
		PowerOK:     true,
		ToleranceOK: true,
	}

	if len(config.PowerRequirements) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range config.PowerRequirements {
		matchingPowers := filterPowers(powers, req)

		for _, power := range matchingPowers {
			// Check value ranges
			if req.MinValue > 0 && power.Value < req.MinValue {
				result.Issues = append(result.Issues,
					fmt.Sprintf("%s: %.3f %s (below min %.3f)", power.Name, power.Value, power.Units, req.MinValue))
				hasErrors = true
				switch power.PowerType {
				case "Voltage":
					result.VoltageOK = false
				case "Current":
					result.CurrentOK = false
				case "Power":
					result.PowerOK = false
				}
			}

			if req.MaxValue > 0 && power.Value > req.MaxValue {
				result.Issues = append(result.Issues,
					fmt.Sprintf("%s: %.3f %s (above max %.3f)", power.Name, power.Value, power.Units, req.MaxValue))
				hasErrors = true
				switch power.PowerType {
				case "Voltage":
					result.VoltageOK = false
				case "Current":
					result.CurrentOK = false
				case "Power":
					result.PowerOK = false
				}
			}

			// Check tolerance
			if req.TolerancePercent > 0 {
				nominalValue := (req.MinValue + req.MaxValue) / 2
				if nominalValue > 0 {
					deviation := ((power.Value - nominalValue) / nominalValue) * 100
					if deviation < 0 {
						deviation = -deviation
					}
					if deviation > req.TolerancePercent {
						result.Issues = append(result.Issues,
							fmt.Sprintf("%s: %.1f%% deviation (tolerance %.1f%%)", power.Name, deviation, req.TolerancePercent))
						result.ToleranceOK = false
						hasWarnings = true
					}
				}
			}

			// Check expected status
			if expectedStatus, exists := req.ExpectedStatus[power.Position]; exists {
				if power.Status != expectedStatus {
					if expectedStatus == "N/A" || power.Status == "N/A" {
						result.Issues = append(result.Issues,
							fmt.Sprintf("%s: status %s (expected %s)", power.Name, power.Status, expectedStatus))
						hasWarnings = true
					} else {
						result.Issues = append(result.Issues,
							fmt.Sprintf("%s: status %s (expected %s)", power.Name, power.Status, expectedStatus))
						hasErrors = true
					}
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
	if len(text) >= width {
		return text[:width]
	}
	spaces := width - len(text)
	left := spaces / 2
	right := spaces - left
	return strings.Repeat(" ", left) + text + strings.Repeat(" ", right)
}

func visualizeSlots(powers []PowerInfo) error {
	printInfo("Power Sensors Layout:")
	fmt.Println()

	// Group by category for better visualization
	categories := make(map[string][]PowerInfo)
	for _, power := range powers {
		categories[power.Category] = append(categories[power.Category], power)
	}

	// Display each category
	for category, categoryPowers := range categories {
		fmt.Printf("\n%s=== %s SENSORS ===%s\n", ColorBlue, category, ColorReset)

		if len(categoryPowers) == 0 {
			continue
		}

		maxSlots := len(categoryPowers)
		if maxSlots > 8 {
			maxSlots = 8 // Limit to 8 per row
		}
		width := 12

		// Top border
		fmt.Print("┌")
		for i := 0; i < maxSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < maxSlots-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Power type row
		fmt.Print("│")
		for i := 0; i < maxSlots && i < len(categoryPowers); i++ {
			power := categoryPowers[i]
			typeText := centerText(power.PowerType, width)

			switch power.PowerType {
			case "Voltage":
				fmt.Print(ColorBlue + typeText + ColorReset)
			case "Current":
				fmt.Print(ColorYellow + typeText + ColorReset)
			case "Power":
				fmt.Print(ColorRed + typeText + ColorReset)
			default:
				fmt.Print(typeText)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Middle separator
		fmt.Print("├")
		for i := 0; i < maxSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < maxSlots-1 {
				fmt.Print("┼")
			}
		}
		fmt.Println("┤")

		// Value row
		fmt.Print("│")
		for i := 0; i < maxSlots && i < len(categoryPowers); i++ {
			power := categoryPowers[i]
			valueText := fmt.Sprintf("%.2f %s", power.Value, power.Units)
			if len(valueText) > width {
				valueText = fmt.Sprintf("%.1f%s", power.Value, power.Units)
			}
			centeredValue := centerText(valueText, width)

			if power.Status == "OK" {
				fmt.Print(ColorGreen + centeredValue + ColorReset)
			} else if power.Status == "WARNING" {
				fmt.Print(ColorYellow + centeredValue + ColorReset)
			} else if power.Status == "CRITICAL" {
				fmt.Print(ColorRed + centeredValue + ColorReset)
			} else {
				fmt.Print(centeredValue)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Bottom border
		fmt.Print("└")
		for i := 0; i < maxSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < maxSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Position labels
		fmt.Print(" ")
		for i := 0; i < maxSlots && i < len(categoryPowers); i++ {
			power := categoryPowers[i]
			fmt.Print(centerText(power.Position, width+1))
		}
		fmt.Println()
	}

	return nil
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning IPMI for power sensors to create configuration...")

	powers, err := getPowerInfo(30)
	if err != nil {
		return fmt.Errorf("could not scan power sensors via IPMI: %v", err)
	}

	if len(powers) == 0 {
		return fmt.Errorf("no power sensors found - cannot create configuration")
	}

	// Create default configuration
	config := Config{
		IPMITimeout: 30,
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
		Visualization: VisualizationConfig{
			TotalSlots: len(powers),
			SlotWidth:  12,
			CategoryVisuals: map[string]PowerVisual{
				"PSU":     generatePowerVisualByCategory("PSU"),
				"CPU":     generatePowerVisualByCategory("CPU"),
				"Memory":  generatePowerVisualByCategory("Memory"),
				"System":  generatePowerVisualByCategory("System"),
				"Chipset": generatePowerVisualByCategory("Chipset"),
				"Battery": generatePowerVisualByCategory("Battery"),
			},
		},
	}

	// Save configuration
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// === MAIN FUNCTION ===

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "power_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected power sensors without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual power layout")
		testIPMIFlag = flag.Bool("test", false, "Test IPMI connection and show basic info")
		debugFlag    = flag.Bool("d", false, "Show detailed debug information")
	)

	flag.Parse()

	debugMode = *debugFlag

	if *showHelpFlag {
		showHelp()
		return
	}

	if *showVersion {
		fmt.Println(VERSION)
		return
	}

	if *testIPMIFlag {
		err := testIPMI()
		if err != nil {
			printError(fmt.Sprintf("IPMI test failed: %v", err))
			os.Exit(1)
		}
		printSuccess("IPMI test completed successfully")
		return
	}

	if *listOnly {
		printInfo("Scanning for power sensors via IPMI...")
		powers, err := getPowerInfo(30)
		if err != nil {
			printError(fmt.Sprintf("Error getting power information via IPMI: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}

		if len(powers) == 0 {
			printWarning("No power sensors found via IPMI")
		} else {
			printSuccess(fmt.Sprintf("Found power sensors via IPMI: %d", len(powers)))

			// Group by category for better display
			categories := make(map[string][]PowerInfo)
			for _, power := range powers {
				categories[power.Category] = append(categories[power.Category], power)
			}

			for category, categoryPowers := range categories {
				fmt.Printf("\n%s%s:%s\n", ColorBlue, category, ColorReset)
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

		var _ *Config = config
		err = visualizeSlots(powers)
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
