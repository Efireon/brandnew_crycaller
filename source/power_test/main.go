package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bougou/go-ipmi"
)

const VERSION = "1.0.0"

type PowerInfo struct {
	Name         string  `json:"name"`
	Position     string  `json:"position"` // PSU1, PSU2, CPU1, CPU2, etc.
	Value        float64 `json:"value"`    // текущее значение
	Units        string  `json:"units"`    // Volts, Amps, Watts
	Status       string  `json:"status"`   // OK, FAIL, ALARM, N/A, etc.
	SensorNumber uint8   `json:"sensor_number"`
	PowerType    string  `json:"power_type"`   // Voltage, Current, Power
	Category     string  `json:"category"`     // PSU, CPU, Memory, System, etc.
	RawValue     float64 `json:"raw_value"`    // сырое значение из IPMI
	MinValue     float64 `json:"min_value"`    // threshold LNC
	MaxValue     float64 `json:"max_value"`    // threshold UNC
	CriticalMin  float64 `json:"critical_min"` // threshold LCR
	CriticalMax  float64 `json:"critical_max"` // threshold UCR
}

type PowerRequirement struct {
	Name             string            `json:"name"`
	PowerType        string            `json:"power_type"`        // Voltage, Current, Power
	Category         string            `json:"category"`          // PSU, CPU, Memory, System
	Positions        []string          `json:"positions"`         // конкретные позиции
	MinValue         float64           `json:"min_value"`         // минимальное допустимое значение
	MaxValue         float64           `json:"max_value"`         // максимальное допустимое значение
	TolerancePercent float64           `json:"tolerance_percent"` // процент отклонения от номинала
	ExpectedStatus   map[string]string `json:"expected_status"`   // position -> expected status
	CheckCritical    bool              `json:"check_critical"`    // проверять критические пороги
}

type PowerVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type RowConfig struct {
	Name  string `json:"name"`  // Display name for the row
	Slots string `json:"slots"` // Slot range (e.g., "1-4", "5-8")
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Enable custom row configuration
	Rows    []RowConfig `json:"rows"`    // Custom row definitions
}

type VisualizationConfig struct {
	TypeVisuals    map[string]PowerVisual `json:"type_visuals"`     // power_type -> visual
	PositionToSlot map[string]int         `json:"position_to_slot"` // position -> logical slot number
	TotalSlots     int                    `json:"total_slots"`
	SlotWidth      int                    `json:"slot_width"`
	SlotsPerRow    int                    `json:"slots_per_row"` // Number of slots per row (legacy)
	GroupByType    bool                   `json:"group_by_type"` // группировать по типам (Voltage, Current, Power)
	CustomRows     CustomRowsConfig       `json:"custom_rows"`   // Custom row configuration
}

type Config struct {
	PowerRequirements []PowerRequirement  `json:"power_requirements"`
	Visualization     VisualizationConfig `json:"visualization"`
	CheckTolerances   bool                `json:"check_tolerances"`
	IPMITimeout       int                 `json:"ipmi_timeout_seconds"`
}

type PowerCheckResult struct {
	Status        string // "ok", "warning", "error"
	Issues        []string
	VoltageOK     bool
	CurrentOK     bool
	PowerOK       bool
	ToleranceOK   bool
	VoltageWarn   bool
	CurrentWarn   bool
	PowerWarn     bool
	ToleranceWarn bool
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorCyan   = "\033[36m"
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
	fmt.Println("  -c <path>   Path to configuration file")
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

		value := sdr.Full.SensorValue
		status := normalizeStatus(sdr.Full.SensorStatus)
		units := determinePowerUnits(sdr)

		minVal := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LNC_Raw))
		maxVal := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UNC_Raw))
		critMinVal := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.LCR_Raw))
		critMaxVal := getSafeThreshold(sdr.Full.ConvertReading(sdr.Full.UCR_Raw))

		powers = append(powers, PowerInfo{
			Name:         name,
			Position:     normalizeIPMIPowerPosition(name),
			Value:        value,
			Units:        units,
			Status:       status,
			SensorNumber: num,
			PowerType:    determinePowerType(name, units),
			Category:     determinePowerCategory(name),
			RawValue:     value,
			MinValue:     minVal,
			MaxValue:     maxVal,
			CriticalMin:  critMinVal,
			CriticalMax:  critMaxVal,
		})
	}

	if len(powers) == 0 {
		return nil, fmt.Errorf("no power sensors found via IPMI SDR records")
	}
	printDebug(fmt.Sprintf("Collected %d power sensors via SDR", len(powers)))
	return powers, nil
}

func isPowerSensor(sdr *ipmi.SDR) bool {
	if sdr.Full == nil {
		return false
	}

	// Check sensor name for power-related keywords
	name := strings.ToUpper(sdr.SensorName())
	powerKeywords := []string{
		"VOLT", "VOLTAGE", "VIN", "VOUT", "VCCIN", "VDDQ", "PCH", "BAT",
		"CUR", "CURRENT", "IOUT", "IIN", "AMPS",
		"PWR", "POWER", "PIN", "POUT", "WATTS",
	}

	for _, keyword := range powerKeywords {
		if strings.Contains(name, keyword) {
			return true
		}
	}

	return false
}

func normalizeStatus(status string) string {
	return strings.ToUpper(strings.TrimSpace(status))
}

func getSafeThreshold(threshold float64) float64 {
	// Return threshold as-is for power sensors, unlike fans we need decimal precision
	if threshold > -1000 && threshold < 100000 { // Reasonable limits
		return threshold
	}
	return 0
}

func determinePowerUnits(sdr *ipmi.SDR) string {
	if sdr.Full == nil {
		return "Unknown"
	}

	// Determine from sensor name
	name := strings.ToUpper(sdr.SensorName())
	if strings.Contains(name, "VOLT") || strings.Contains(name, "VIN") ||
		strings.Contains(name, "VOUT") || strings.Contains(name, "VCCIN") ||
		strings.Contains(name, "VDDQ") || strings.Contains(name, "BAT") ||
		strings.Contains(name, "PCH") {
		return "Volts"
	} else if strings.Contains(name, "CUR") || strings.Contains(name, "IOUT") ||
		strings.Contains(name, "IIN") || strings.Contains(name, "AMPS") {
		return "Amps"
	} else if strings.Contains(name, "PWR") || strings.Contains(name, "PIN") ||
		strings.Contains(name, "POUT") || strings.Contains(name, "WATTS") {
		return "Watts"
	}
	return "Unknown"
}

func determinePowerType(name, units string) string {
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
	} else if strings.Contains(name, "CPU") {
		return "CPU"
	} else if strings.Contains(name, "DDR") || strings.Contains(name, "VDDQ") {
		return "Memory"
	} else if strings.Contains(name, "PCH") {
		return "Chipset"
	} else if strings.Contains(name, "BAT") {
		return "Battery"
	} else if strings.Contains(name, "12V") || strings.Contains(name, "5V") ||
		strings.Contains(name, "3.3V") || strings.Contains(name, "VSB") {
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

		// Voltage rail patterns
		{regexp.MustCompile(`VOLT(\d+\.?\d*)V(?:SB)?`), "%sV"},
		{regexp.MustCompile(`(\d+\.?\d*)VSB`), "%sVSB"},
		{regexp.MustCompile(`(\d+\.?\d*)V`), "%sV"},

		// Memory patterns
		{regexp.MustCompile(`VOLTVDDQ([A-P]+)`), "VDDQ_%s"},
		{regexp.MustCompile(`VDDQ([A-P]+)`), "VDDQ_%s"},

		// PCH patterns
		{regexp.MustCompile(`VOLTPCH(\w+)`), "PCH_%s"},
		{regexp.MustCompile(`PCH(\w+)`), "PCH_%s"},

		// Battery
		{regexp.MustCompile(`VOLTBAT`), "BAT"},
		{regexp.MustCompile(`BATTERY`), "BAT"},

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
				result := fmt.Sprintf(pattern.format, parts[0])
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
					fmt.Sprintf("%s: %.3f %s (min %.3f)", power.Name, power.Value, power.Units, req.MinValue))

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
					fmt.Sprintf("%s: %.3f %s (max %.3f)", power.Name, power.Value, power.Units, req.MaxValue))

				if power.PowerType == "Voltage" {
					result.VoltageOK = false
				} else if power.PowerType == "Current" {
					result.CurrentOK = false
				} else if power.PowerType == "Power" {
					result.PowerOK = false
				}
				hasErrors = true
			}

			// Check critical thresholds
			if req.CheckCritical {
				if power.CriticalMin > 0 && power.Value < power.CriticalMin {
					result.Issues = append(result.Issues,
						fmt.Sprintf("%s: CRITICAL LOW %.3f %s (min %.3f)", power.Name, power.Value, power.Units, power.CriticalMin))
					hasErrors = true
				}

				if power.CriticalMax > 0 && power.Value > power.CriticalMax {
					result.Issues = append(result.Issues,
						fmt.Sprintf("%s: CRITICAL HIGH %.3f %s (max %.3f)", power.Name, power.Value, power.Units, power.CriticalMax))
					hasErrors = true
				}
			}

			// Check tolerance
			if config.CheckTolerances && req.TolerancePercent > 0 {
				// Номинальное значение - середина между min и max
				if req.MinValue > 0 && req.MaxValue > 0 {
					nominal := (req.MinValue + req.MaxValue) / 2
					tolerance := nominal * req.TolerancePercent / 100

					if power.Value < (nominal-tolerance) || power.Value > (nominal+tolerance) {
						result.Issues = append(result.Issues,
							fmt.Sprintf("%s: %.3f %s outside tolerance (±%.1f%% of %.3f)",
								power.Name, power.Value, power.Units, req.TolerancePercent, nominal))
						result.ToleranceOK = false
						result.ToleranceWarn = true
						hasWarnings = true
					}
				}
			}

			// Check status
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

			fmt.Printf("  %d. %s: %s%.3f %s%s (%s%s%s)\n",
				i+1, power.Name, ColorCyan, power.Value, power.Units, ColorReset,
				statusColor, power.Status, ColorReset)

			if debugMode {
				printDebug(fmt.Sprintf("    Position: %s, Type: %s, Sensor: %d",
					power.Position, power.PowerType, power.SensorNumber))
				if power.MinValue > 0 || power.MaxValue > 0 {
					printDebug(fmt.Sprintf("    Thresholds: %.3f - %.3f %s",
						power.MinValue, power.MaxValue, power.Units))
				}
			}
		}
	}

	// Check requirements
	result := checkPowerAgainstRequirements(powers, config)

	if len(result.Issues) > 0 {
		for _, issue := range result.Issues {
			if result.Status == "error" {
				printError(fmt.Sprintf("  %s", issue))
			} else if result.Status == "warning" {
				printWarning(fmt.Sprintf("  %s", issue))
			}
		}
	}

	if result.Status == "error" {
		printError("Power requirements FAILED")
		return fmt.Errorf("power requirements not met")
	} else if result.Status == "warning" {
		printWarning("Power requirements passed with warnings")
	} else {
		printSuccess("All power requirements passed")
	}

	return nil
}

// === VISUALIZATION ===

func visualizeSlots(powers []PowerInfo, config *Config) error {
	printInfo("Power Sensors Layout:")
	fmt.Println()

	if config.Visualization.GroupByType {
		return visualizeByType(powers, config)
	}

	return visualizeFlat(powers, config)
}

func visualizeByType(powers []PowerInfo, config *Config) error {
	types := []string{"Voltage", "Current", "Power"}

	for _, powerType := range types {
		typePowers := make([]PowerInfo, 0)
		for _, power := range powers {
			if power.PowerType == powerType {
				typePowers = append(typePowers, power)
			}
		}

		if len(typePowers) == 0 {
			continue
		}

		fmt.Printf("\n%s%s Sensors:%s\n", ColorBlue, powerType, ColorReset)

		for _, power := range typePowers {
			visual := getPowerVisual(power, config)
			statusColor := getStatusColor(power.Status)

			fmt.Printf("[%s%s%s] %s%s%s - %.3f %s (%s%s%s)\n",
				getANSIColor(visual.Color), visual.Symbol, ColorReset,
				ColorCyan, power.Name, ColorReset,
				power.Value, power.Units,
				statusColor, power.Status, ColorReset)
		}
	}

	return nil
}

func visualizeFlat(powers []PowerInfo, config *Config) error {
	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(powers)
	}

	// Create position to power mapping
	posToPosition := make(map[int]string, len(config.Visualization.PositionToSlot))
	for position, pos := range config.Visualization.PositionToSlot {
		posToPosition[pos] = position
	}

	// Fill slot data array
	slotData := make([]PowerInfo, maxSlots+1)
	for _, power := range powers {
		if pos, ok := config.Visualization.PositionToSlot[power.Position]; ok && pos >= 1 && pos <= maxSlots {
			slotData[pos] = power
		}
	}

	// System check for coloring
	checkPowerAgainstRequirements(powers, config)

	// Legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Normal Value    ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Warning/Issue   ", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %sMISS%s Missing Sensor ", ColorRed, ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorGray, "░░░", ColorReset)
	fmt.Println()

	// Generate rows (custom or legacy)
	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		perRow := config.Visualization.SlotsPerRow
		if perRow == 0 {
			perRow = 8
		}
		for start := 1; start <= maxSlots; start += perRow {
			end := start + perRow - 1
			if end > maxSlots {
				end = maxSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Power Bank %d", len(rows)+1),
				Slots: fmt.Sprintf("%d-%d", start, end),
			})
		}
	}

	// Visualize each row
	for _, row := range rows {
		start, end, err := parseSlotRange(row.Slots)
		if err != nil || start < 1 || end > maxSlots {
			printWarning(fmt.Sprintf("Skipping invalid row '%s': %v", row.Slots, err))
			continue
		}
		count := end - start + 1
		width := config.Visualization.SlotWidth
		if width < 8 {
			width = 8
		}

		// Row header
		if len(rows) > 1 {
			fmt.Printf("%s (Slots %d-%d):\n", row.Name, start, end)
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

		// Symbols row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			power := slotData[idx]

			if power.Name != "" {
				visual := getPowerVisual(power, config)
				sym := centerText(visual.Symbol, width)
				color := getStatusColor(power.Status)
				fmt.Print(color + sym + ColorReset)
			} else {
				position := posToPosition[idx]
				if isRequiredPosition(position, config.PowerRequirements) {
					miss := centerText("MISS", width)
					fmt.Print(ColorRed + miss + ColorReset)
				} else {
					empty := centerText("░░░", width)
					fmt.Print(ColorGray + empty + ColorReset)
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Type/Position row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			power := slotData[idx]
			if power.Name != "" {
				// Показываем имя сенсора вместо типа
				sensorName := shortenSensorName(power.Name)
				txt := centerText(sensorName, width)
				color := getStatusColor(power.Status)
				fmt.Print(color + txt + ColorReset)
			} else {
				position := posToPosition[idx]
				if isRequiredPosition(position, config.PowerRequirements) {
					txt := centerText("REQ", width)
					fmt.Print(ColorRed + txt + ColorReset)
				} else {
					fmt.Print(centerText("", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Value row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			power := slotData[idx]
			if power.Name != "" {
				valueStr := formatValue(power.Value, power.Units)
				if len(valueStr) > width-1 {
					valueStr = valueStr[:width-1]
				}

				txt := centerText(valueStr, width)
				color := getStatusColor(power.Status)
				fmt.Print(color + txt + ColorReset)
			} else {
				fmt.Print(centerText("", width))
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
		fmt.Println()
	}

	return nil
}

func getPowerVisual(power PowerInfo, config *Config) PowerVisual {
	if visual, exists := config.Visualization.TypeVisuals[power.PowerType]; exists {
		return visual
	}

	return generatePowerVisual(power.PowerType)
}

func generatePowerVisual(powerType string) PowerVisual {
	visual := PowerVisual{
		Description: fmt.Sprintf("%s Sensor", powerType),
		Color:       "green",
	}

	switch powerType {
	case "Voltage":
		visual.Symbol = "▓▓▓"
		visual.ShortName = "VOLT"
	case "Current":
		visual.Symbol = "═══"
		visual.ShortName = "CURR"
	case "Power":
		visual.Symbol = "███"
		visual.ShortName = "PWR"
	default:
		visual.Symbol = "░░░"
		visual.ShortName = "UNK"
	}

	return visual
}

func getStatusColor(status string) string {
	switch strings.ToUpper(status) {
	case "OK":
		return ColorGreen
	case "WARN", "WARNING":
		return ColorYellow
	case "FAIL", "ERROR", "CRITICAL":
		return ColorRed
	case "N/A", "NA":
		return ColorGray
	default:
		return ColorWhite
	}
}

func getANSIColor(name string) string {
	switch strings.ToLower(name) {
	case "green":
		return ColorGreen
	case "blue":
		return ColorBlue
	case "yellow":
		return ColorYellow
	case "red":
		return ColorRed
	case "gray", "grey":
		return ColorGray
	case "white":
		return ColorWhite
	case "cyan":
		return ColorCyan
	default:
		return ColorWhite
	}
}

func parseSlotRange(slotRange string) (int, int, error) {
	// Parse slot ranges like "1-4", "5-8", etc.
	parts := strings.Split(slotRange, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid slot range format: %s", slotRange)
	}

	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start slot: %s", parts[0])
	}

	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end slot: %s", parts[1])
	}

	if start > end {
		return 0, 0, fmt.Errorf("start slot %d is greater than end slot %d", start, end)
	}

	return start, end, nil
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

func shortenSensorName(name string) string {
	// Убираем общие префиксы
	cleaned := name
	prefixes := []string{"VOLT_", "CUR_", "PWR_", "TEMP_"}

	for _, prefix := range prefixes {
		if strings.HasPrefix(cleaned, prefix) {
			cleaned = cleaned[len(prefix):]
			break
		}
	}

	// Применяем сокращения для читаемости
	replacements := map[string]string{
		"PSU1_VIN":   "PSU1IN",
		"PSU2_VIN":   "PSU2IN",
		"PSU1_IOUT":  "PSU1A",
		"PSU2_IOUT":  "PSU2A",
		"PSU1_PIN":   "PSU1W",
		"PSU2_PIN":   "PSU2W",
		"PSU1_POUT":  "PSU1W",
		"PSU2_POUT":  "PSU2W",
		"CPU1_VCCIN": "CPU1",
		"CPU2_VCCIN": "CPU2",
		"VDDQ_ABCD":  "DDRA",
		"VDDQ_EFGH":  "DDRE",
		"VDDQ_IJKL":  "DDRI",
		"VDDQ_MNOP":  "DDRM",
		"PCH_PVNN":   "PCHP",
		"PCH_1.05V":  "PCH105",
		"PCH_1.8V":   "PCH18",
		"3.3VSB":     "3.3SB",
		"5VSB":       "5VSB",
		"BAT":        "BATT",
		"12V":        "12V",
		"5V":         "5V",
		"3.3V":       "3.3V",
	}

	if replacement, exists := replacements[cleaned]; exists {
		cleaned = replacement
	}

	// Если имя длинное, пытаемся сократить его разумно
	if len(cleaned) > 8 {
		// Убираем подчеркивания и лишние символы
		cleaned = strings.ReplaceAll(cleaned, "_", "")

		// Если все еще длинное, берем первые 8 символов
		if len(cleaned) > 8 {
			cleaned = cleaned[:8]
		}
	}

	return cleaned
}

func isRequiredPosition(position string, requirements []PowerRequirement) bool {
	for _, req := range requirements {
		for _, pos := range req.Positions {
			if pos == position {
				return true
			}
		}
	}
	return false
}

func generateDefaultCustomRows(totalSlots int) CustomRowsConfig {
	if totalSlots <= 8 {
		return CustomRowsConfig{
			Enabled: false,
			Rows: []RowConfig{
				{Name: "Power Rail 1", Slots: "1-8"},
			},
		}
	}

	var rows []RowConfig
	slotsPerRow := 8

	for start := 1; start <= totalSlots; start += slotsPerRow {
		end := start + slotsPerRow - 1
		if end > totalSlots {
			end = totalSlots
		}

		rows = append(rows, RowConfig{
			Name:  fmt.Sprintf("Power Rail %d", len(rows)+1),
			Slots: fmt.Sprintf("%d-%d", start, end),
		})
	}

	return CustomRowsConfig{
		Enabled: false, // Disabled by default
		Rows:    rows,
	}
}

func getShortUnits(units string) string {
	switch units {
	case "Volts":
		return "V"
	case "Amps":
		return "A"
	case "Watts":
		return "W"
	default:
		return units
	}
}

func formatValue(value float64, units string) string {
	shortUnits := getShortUnits(units)

	// Форматируем в зависимости от размера значения
	if value >= 100.0 {
		return fmt.Sprintf("%.0f%s", value, shortUnits)
	} else if value >= 10.0 {
		return fmt.Sprintf("%.1f%s", value, shortUnits)
	} else {
		return fmt.Sprintf("%.2f%s", value, shortUnits)
	}
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
					fmt.Printf("  %d. %s\n", i+1, power.Name)
					fmt.Printf("     Position: %s\n", power.Position)
					fmt.Printf("     Type: %s\n", power.PowerType)
					fmt.Printf("     Current Value: %.3f %s\n", power.Value, power.Units)
					fmt.Printf("     Status: %s\n", power.Status)
					fmt.Printf("     Sensor Number: %d\n", power.SensorNumber)
					if power.MinValue > 0 || power.MaxValue > 0 {
						fmt.Printf("     Thresholds: %.3f - %.3f %s\n",
							power.MinValue, power.MaxValue, power.Units)
					}
					if power.CriticalMin > 0 || power.CriticalMax > 0 {
						fmt.Printf("     Critical: %.3f - %.3f %s\n",
							power.CriticalMin, power.CriticalMax, power.Units)
					}
					fmt.Printf("     Raw Value: %.2f\n", power.RawValue)
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

	// Default: load configuration and perform power check
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

// === CONFIG CREATION ===

func createDefaultConfig(configPath string) error {
	printInfo("Detecting power sensors via IPMI...")
	powers, err := getPowerInfo(30)
	if err != nil {
		return fmt.Errorf("failed to get power info: %v", err)
	}

	if len(powers) == 0 {
		return fmt.Errorf("no power sensors found - cannot create meaningful configuration")
	}

	// Group sensors by category and type
	categoryGroups := make(map[string]map[string][]PowerInfo)
	for _, power := range powers {
		if categoryGroups[power.Category] == nil {
			categoryGroups[power.Category] = make(map[string][]PowerInfo)
		}
		categoryGroups[power.Category][power.PowerType] = append(
			categoryGroups[power.Category][power.PowerType], power)
	}

	var requirements []PowerRequirement
	positionMapping := make(map[string]int)

	slot := 1
	for category, typeGroups := range categoryGroups {
		for powerType, typePowers := range typeGroups {
			var positions []string
			expectedStatus := make(map[string]string)

			for _, power := range typePowers {
				positions = append(positions, power.Position)
				expectedStatus[power.Position] = power.Status
				positionMapping[power.Position] = slot
				slot++
			}

			// Create reasonable default ranges based on detected values
			var minVal, maxVal float64
			if len(typePowers) > 0 {
				first := typePowers[0]
				if first.MinValue > 0 {
					minVal = first.MinValue
				} else {
					minVal = first.Value * 0.9 // 10% tolerance
				}
				if first.MaxValue > 0 {
					maxVal = first.MaxValue
				} else {
					maxVal = first.Value * 1.1 // 10% tolerance
				}
			}

			req := PowerRequirement{
				Name:             fmt.Sprintf("%s %s (%d sensors)", category, powerType, len(typePowers)),
				PowerType:        powerType,
				Category:         category,
				Positions:        positions,
				MinValue:         minVal,
				MaxValue:         maxVal,
				TolerancePercent: 5.0, // 5% tolerance by default
				ExpectedStatus:   expectedStatus,
				CheckCritical:    true,
			}
			requirements = append(requirements, req)
		}
	}

	// Create type visuals
	typeVisuals := map[string]PowerVisual{
		"Voltage": {
			Symbol:      "▓▓▓",
			ShortName:   "VOLT",
			Description: "Voltage Sensor",
			Color:       "blue",
		},
		"Current": {
			Symbol:      "═══",
			ShortName:   "CURR",
			Description: "Current Sensor",
			Color:       "yellow",
		},
		"Power": {
			Symbol:      "███",
			ShortName:   "PWR",
			Description: "Power Sensor",
			Color:       "green",
		},
	}

	config := Config{
		PowerRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:    typeVisuals,
			PositionToSlot: positionMapping,
			TotalSlots:     len(powers),
			SlotWidth:      12,                                     // Ширина слота для отображения
			SlotsPerRow:    8,                                      // Legacy fallback
			GroupByType:    true,                                   // Group by type by default
			CustomRows:     generateDefaultCustomRows(len(powers)), // Generate custom rows
		},
		CheckTolerances: true,
		IPMITimeout:     30,
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0755)
	if err != nil {
		return err
	}

	err = os.WriteFile(configPath, data, 0644)
	if err != nil {
		return err
	}

	printSuccess("Configuration created successfully based on detected power sensors")
	printInfo(fmt.Sprintf("Total power sensors: %d", len(powers)))
	printInfo(fmt.Sprintf("Categories found: %d", len(categoryGroups)))

	// Print summary by category
	for category, typeGroups := range categoryGroups {
		totalInCategory := 0
		for _, typePowers := range typeGroups {
			totalInCategory += len(typePowers)
		}
		printInfo(fmt.Sprintf("  %s: %d sensors", category, totalInCategory))
	}

	printInfo("Requirements configured with 5% tolerance by default")
	printInfo("Visualization grouped by type by default")
	printInfo("Custom row layout generated (disabled by default)")
	printInfo("To enable custom rows: set 'visualization.custom_rows.enabled' to true")
	printInfo("You can edit the configuration file to adjust thresholds, requirements, and row layout")

	return nil
}
