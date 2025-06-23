package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bougou/go-ipmi"
)

const VERSION = "2.0.1"

type FanInfo struct {
	Name         string  `json:"name"`
	Position     string  `json:"position"` // CPU1, CHS1, PSU1, FAN1_1, etc.
	CurrentRPM   int     `json:"current_rpm"`
	TargetRPM    int     `json:"target_rpm"` // если доступно
	MinRPM       int     `json:"min_rpm"`    // threshold LNC
	MaxRPM       int     `json:"max_rpm"`    // threshold UNC
	Status       string  `json:"status"`     // OK, FAIL, ALARM, N/A, etc.
	SensorNumber uint8   `json:"sensor_number"`
	FanType      string  `json:"fan_type"`  // CPU, Chassis, PSU, PCIe, etc.
	RawValue     float64 `json:"raw_value"` // сырое значение из IPMI
	Units        string  `json:"units"`     // единицы измерения
}

type FanRequirement struct {
	Name           string            `json:"name"`
	MinRPM         int               `json:"min_rpm"`
	MaxRPM         int               `json:"max_rpm"`
	FanType        string            `json:"fan_type"`        // CPU, Chassis, PSU, PCIe
	Positions      []string          `json:"positions"`       // конкретные позиции
	MinCount       int               `json:"min_count"`       // минимальное количество активных вентиляторов
	MaxRPMDiff     int               `json:"max_rpm_diff"`    // максимальное отклонение от target
	ExpectedStatus map[string]string `json:"expected_status"` // position -> expected status (OK, N/A, etc.)
}

type FanVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type VisualizationConfig struct {
	TypeVisuals    map[string]FanVisual `json:"type_visuals"`     // fan_type -> visual (CPU, Chassis, PSU, etc.)
	PositionToSlot map[string]int       `json:"position_to_slot"` // position -> logical slot number
	TotalSlots     int                  `json:"total_slots"`
	SlotWidth      int                  `json:"slot_width"`
	IgnoreRows     []string             `json:"ignore_rows"`   // patterns to ignore during visualization, e.g. ["_2"]
	MaxRowSlots    int                  `json:"max_row_slots"` // максимальное количество слотов в одном ряду для многорядной визуализации
}

type Config struct {
	FanRequirements []FanRequirement    `json:"fan_requirements"`
	Visualization   VisualizationConfig `json:"visualization"`
	CheckTargetRPM  bool                `json:"check_target_rpm"`
	IPMITimeout     int                 `json:"ipmi_timeout_seconds"` // таймаут для IPMI операций
}

type FanCheckResult struct {
	Status     string // "ok", "warning", "error", "missing"
	Issues     []string
	RPMOK      bool
	StatusOK   bool
	RPMWarn    bool
	StatusWarn bool
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
)

var (
	fanSDRIndices []int
	fanSDROnce    sync.Once
	fanSDRMutex   sync.Mutex
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
	fmt.Printf("Fan Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected fans without configuration check")
	fmt.Println("  -vis        Show visual fan layout")
	fmt.Println("  -multirow   Show visual layout in multiple rows")
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

// === FAN DETECTION USING IPMI ONLY ===

// getFanInfo concurrently invokes detection methods and returns the first non-empty result.
func getFanInfo(timeoutSec int) ([]FanInfo, error) {
	printDebug("Starting IPMI fan detection via SDR enhancement...")

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

	// Cache fan SDR indices once
	fanSDROnce.Do(func() {
		fanSDRMutex.Lock()
		defer fanSDRMutex.Unlock()
		for i, sdr := range sdrs {
			if sdr.Full != nil && sdr.HasAnalogReading() && sdr.Full.SensorType == ipmi.SensorTypeFan {
				fanSDRIndices = append(fanSDRIndices, i)
			}
		}
		printDebug(fmt.Sprintf("Cached %d fan SDR indices", len(fanSDRIndices)))
	})

	// Prepare slice for fans
	fans := make([]FanInfo, 0, len(fanSDRIndices))

	// Copy indices under lock
	fanSDRMutex.Lock()
	idxs := append([]int(nil), fanSDRIndices...)
	fanSDRMutex.Unlock()

	// Read each fan from SDRs
	for _, idx := range idxs {
		if idx < 0 || idx >= len(sdrs) {
			continue
		}
		sdr := sdrs[idx]
		name := sdr.SensorName()
		num := uint8(sdr.SensorNumber())

		raw := sdr.Full.SensorValue
		status := normalizeStatus(sdr.Full.SensorStatus)

		minVal := sdr.Full.ConvertReading(sdr.Full.LNC_Raw)
		maxVal := sdr.Full.ConvertReading(sdr.Full.UNC_Raw)

		fans = append(fans, FanInfo{
			Name:         name,
			Position:     normalizeIPMIFanPosition(name),
			FanType:      determineIPMIFanType(name),
			SensorNumber: num,
			RawValue:     raw,
			Units:        "RPM",
			CurrentRPM:   int(raw),
			Status:       status,
			MinRPM:       getSafeThreshold(minVal),
			MaxRPM:       getSafeThreshold(maxVal),
		})
	}

	if len(fans) == 0 {
		return nil, fmt.Errorf("no fan sensors found via IPMI SDR records")
	}
	printDebug(fmt.Sprintf("Collected %d fans via enhanced SDR", len(fans)))
	return fans, nil
}

func isFanSensor(sensorName string) bool {
	name := strings.ToUpper(sensorName)
	fanKeywords := []string{"FAN", "COOLING", "COOLER"}

	for _, keyword := range fanKeywords {
		if strings.Contains(name, keyword) {
			return true
		}
	}

	return false
}

func normalizeStatus(status string) string {
	// Приводим все статусы к верхнему регистру для единообразия
	return strings.ToUpper(strings.TrimSpace(status))
}

func getSafeThreshold(threshold float64) int {
	// Безопасное преобразование пороговых значений
	if threshold > 0 && threshold < 50000 { // Разумные пределы для RPM
		return int(threshold)
	}
	return 0
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
		return fmt.Errorf("SDR access required for fan detection")
	} else {
		printSuccess(fmt.Sprintf("SDR access OK: found %d records", len(sdrs)))

		// Показываем статистику по типам сенсоров
		fanCount := 0
		for _, sdr := range sdrs {
			if isFanSensor(sdr.SensorName()) {
				fanCount++
			}
		}
		printInfo(fmt.Sprintf("  Fan-related sensors found: %d", fanCount))
	}

	return nil
}

// === HELPER FUNCTIONS ===

func normalizeIPMIFanPosition(fanName string) string {
	name := strings.ToUpper(fanName)
	name = strings.ReplaceAll(name, " ", "")

	// Обработка различных паттернов имен IPMI вентиляторов
	patterns := []struct {
		regex  *regexp.Regexp
		format string
	}{
		{regexp.MustCompile(`FAN(\d+)(?:_(\d+))?`), "FAN%s%s"},
		{regexp.MustCompile(`CPU.*?FAN(\d*)`), "CPU%s"},
		{regexp.MustCompile(`CPU(\d+)`), "CPU%s"},
		{regexp.MustCompile(`PSU(\d*)`), "PSU%s"},
		{regexp.MustCompile(`CHASSIS.*?(\d+)`), "CHS%s"},
		{regexp.MustCompile(`SYS.*?FAN(\d+)`), "CHS%s"},
	}

	for _, pattern := range patterns {
		if matches := pattern.regex.FindStringSubmatch(name); len(matches) > 1 {
			var parts []string
			for _, match := range matches[1:] {
				if match != "" {
					parts = append(parts, match)
				}
			}

			switch pattern.format {
			case "FAN%s%s":
				if len(parts) >= 2 {
					return fmt.Sprintf("FAN%s_%s", parts[0], parts[1])
				} else if len(parts) == 1 {
					return fmt.Sprintf("FAN%s", parts[0])
				}
			case "CPU%s":
				if len(parts) > 0 {
					return fmt.Sprintf("CPU%s", parts[0])
				} else {
					return "CPU1"
				}
			case "PSU%s":
				if len(parts) > 0 {
					return fmt.Sprintf("PSU%s", parts[0])
				} else {
					return "PSU1"
				}
			case "CHS%s":
				if len(parts) > 0 {
					return fmt.Sprintf("CHS%s", parts[0])
				} else {
					return "CHS1"
				}
			}
		}
	}

	// Fallback: используем исходное имя
	return name
}

func determineIPMIFanType(fanName string) string {
	name := strings.ToUpper(fanName)

	typePatterns := []struct {
		keywords []string
		fanType  string
	}{
		{[]string{"CPU"}, "CPU"},
		{[]string{"PSU", "POWER"}, "PSU"},
		{[]string{"CHASSIS", "SYS", "CASE"}, "Chassis"},
		{[]string{"PCI", "GPU"}, "PCIe"},
		{[]string{"FAN"}, "Chassis"}, // Default for generic fans
	}

	for _, pattern := range typePatterns {
		for _, keyword := range pattern.keywords {
			if strings.Contains(name, keyword) {
				return pattern.fanType
			}
		}
	}

	return "Other"
}

func generateFanVisualByType(fanType string) FanVisual {
	visual := FanVisual{
		Description: fmt.Sprintf("%s Fan", fanType),
		Color:       "green",
	}

	switch fanType {
	case "CPU":
		visual.Symbol = "▓▓▓"
		visual.ShortName = "CPU"
	case "Chassis":
		visual.Symbol = "═══"
		visual.ShortName = "CHS"
	case "PSU":
		visual.Symbol = "███"
		visual.ShortName = "PSU"
	case "PCIe":
		visual.Symbol = "≡≡≡"
		visual.ShortName = "PCIe"
	default:
		visual.Symbol = "░░░"
		visual.ShortName = "FAN"
	}

	return visual
}

func analyzeFanRows(fans []FanInfo) map[string][]string {
	rowAnalysis := make(map[string][]string)

	for _, fan := range fans {
		if strings.Contains(fan.Position, "_") {
			parts := strings.Split(fan.Position, "_")
			if len(parts) == 2 {
				rowSuffix := "_" + parts[1]
				rowAnalysis[rowSuffix] = append(rowAnalysis[rowSuffix], fan.Position)
			}
		}
	}

	return rowAnalysis
}

func suggestIgnoreRows(rowAnalysis map[string][]string) []string {
	var ignoreRows []string

	row1Count := len(rowAnalysis["_1"])
	row2Count := len(rowAnalysis["_2"])

	if row1Count > 0 && row2Count > 0 {
		if row2Count < row1Count {
			ignoreRows = append(ignoreRows, "_2")
		}
		printInfo(fmt.Sprintf("Detected fan rows: _1 (%d fans), _2 (%d fans)", row1Count, row2Count))
	}

	return ignoreRows
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

func formatRPM(rpm int) string {
	if rpm == 0 {
		return "0"
	}

	if rpm < 1000 {
		return fmt.Sprintf("%d", rpm)
	} else {
		return fmt.Sprintf("%.1fk", float64(rpm)/1000.0)
	}
}

func findFanByPosition(fans []FanInfo, position string) *FanInfo {
	for _, fan := range fans {
		if fan.Position == position {
			return &fan
		}
	}
	return nil
}

func filterFansByIgnorePatterns(fans []FanInfo, ignorePatterns []string) []FanInfo {
	if len(ignorePatterns) == 0 {
		return fans
	}

	var filtered []FanInfo

	for _, fan := range fans {
		shouldIgnore := false
		for _, pattern := range ignorePatterns {
			if strings.Contains(fan.Position, pattern) {
				shouldIgnore = true
				break
			}
		}

		if !shouldIgnore {
			filtered = append(filtered, fan)
		} else if debugMode {
			printDebug(fmt.Sprintf("Ignoring fan %s due to pattern match", fan.Position))
		}
	}

	return filtered
}

func filterPositionToSlotByIgnorePatterns(positionToSlot map[string]int, ignorePatterns []string) map[string]int {
	if len(ignorePatterns) == 0 {
		return positionToSlot
	}

	filtered := make(map[string]int)

	for position, slot := range positionToSlot {
		shouldIgnore := false
		for _, pattern := range ignorePatterns {
			if strings.Contains(position, pattern) {
				shouldIgnore = true
				if debugMode {
					printDebug(fmt.Sprintf("Removing position %s (slot %d) from visualization due to ignore pattern", position, slot))
				}
				break
			}
		}

		if !shouldIgnore {
			filtered[position] = slot
		}
	}

	// Пересчитываем слоты для непрерывной нумерации
	var positions []string
	for position := range filtered {
		positions = append(positions, position)
	}

	// Сортируем позиции по исходным номерам слотов
	for i := 0; i < len(positions)-1; i++ {
		for j := i + 1; j < len(positions); j++ {
			if positionToSlot[positions[i]] > positionToSlot[positions[j]] {
				positions[i], positions[j] = positions[j], positions[i]
			}
		}
	}

	// Присваиваем новые последовательные номера слотов
	result := make(map[string]int)
	for i, position := range positions {
		result[position] = i + 1
	}

	return result
}

func getFanVisualByType(fan FanInfo, config *VisualizationConfig) FanVisual {
	if fan.Name == "" {
		return FanVisual{
			Symbol:      "░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	if visual, exists := config.TypeVisuals[fan.FanType]; exists {
		return visual
	}

	return generateFanVisualByType(fan.FanType)
}

func getExpectedStatus(position string, requirements []FanRequirement) string {
	for _, req := range requirements {
		if expectedStatus, exists := req.ExpectedStatus[position]; exists {
			return expectedStatus
		}
	}
	return ""
}

func filterFans(fans []FanInfo, req FanRequirement) []FanInfo {
	var matching []FanInfo

	for _, fan := range fans {
		if len(req.Positions) > 0 {
			found := false
			for _, position := range req.Positions {
				if fan.Position == position {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		} else {
			if req.FanType != "" && req.FanType != "any" && fan.FanType != req.FanType {
				continue
			}
		}

		matching = append(matching, fan)
	}

	return matching
}

// === CONFIGURATION FUNCTIONS ===

func createDefaultConfig(configPath string) error {
	printInfo("Scanning IPMI for fans to create configuration...")

	cfg, err := loadConfig(configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found fans")
		printInfo("Use -test to diagnose IPMI connectivity issues")
		os.Exit(1)
	}

	allFans, err := getFanInfo(cfg.IPMITimeout)
	if err != nil {
		return fmt.Errorf("could not scan fans via IPMI: %v", err)
	}

	if len(allFans) == 0 {
		printError("No fans found via IPMI")
		return fmt.Errorf("no fans found - cannot create configuration")
	}

	// Разделяем активные и недоступные вентиляторы
	activeFans := []FanInfo{}
	naFans := []FanInfo{}

	for _, fan := range allFans {
		if fan.Status == "N/A" {
			naFans = append(naFans, fan)
		} else {
			activeFans = append(activeFans, fan)
		}
	}

	printInfo(fmt.Sprintf("Found %d total fan positions via IPMI:", len(allFans)))
	printInfo(fmt.Sprintf("  - %d active fans", len(activeFans)))
	printInfo(fmt.Sprintf("  - %d physically absent (N/A) fans", len(naFans)))

	// Анализируем паттерны вентиляторов для предложений по игнорированию
	rowAnalysis := analyzeFanRows(allFans)

	var requirements []FanRequirement
	typeVisuals := make(map[string]FanVisual)
	positionToSlot := make(map[string]int)

	// Создаем мэппинг позиций на слоты для ВСЕХ вентиляторов
	for i, fan := range allFans {
		logicalSlot := i + 1
		positionToSlot[fan.Position] = logicalSlot
		printInfo(fmt.Sprintf("  Mapping %s -> Slot %d (Sensor: %d, Status: %s)",
			fan.Position, logicalSlot, fan.SensorNumber, fan.Status))
	}

	// Группируем вентиляторы по типу
	typeGroups := make(map[string][]FanInfo)
	for _, fan := range allFans {
		typeGroups[fan.FanType] = append(typeGroups[fan.FanType], fan)
	}

	// Создаем требования и визуалы по типам
	createdVisuals := make(map[string]bool)

	for fanType, fansOfType := range typeGroups {
		activeFansOfType := []FanInfo{}
		naFansOfType := []FanInfo{}

		for _, fan := range fansOfType {
			if fan.Status == "N/A" {
				naFansOfType = append(naFansOfType, fan)
			} else {
				activeFansOfType = append(activeFansOfType, fan)
			}
		}

		printInfo(fmt.Sprintf("  Processing %d %s fans (%d active, %d N/A):",
			len(fansOfType), fanType, len(activeFansOfType), len(naFansOfType)))

		var positions []string
		expectedStatus := make(map[string]string)
		minRPM := 0
		maxRPM := 0

		for _, fan := range fansOfType {
			positions = append(positions, fan.Position)
			expectedStatus[fan.Position] = normalizeStatus(fan.Status)

			if fan.Status != "N/A" {
				printInfo(fmt.Sprintf("    - %s: %d RPM (Sensor ID: %d)", fan.Name, fan.CurrentRPM, fan.SensorNumber))
				if fan.MinRPM > 0 && (minRPM == 0 || fan.MinRPM < minRPM) {
					minRPM = fan.MinRPM
				}
				if fan.MaxRPM > maxRPM {
					maxRPM = fan.MaxRPM
				}
			} else {
				printInfo(fmt.Sprintf("    - %s: N/A (physically absent, Sensor ID: %d)", fan.Name, fan.SensorNumber))
			}
		}

		// Создаем визуальный паттерн для этого типа (один раз на тип)
		if !createdVisuals[fanType] {
			visual := generateFanVisualByType(fanType)
			typeVisuals[fanType] = visual
			createdVisuals[fanType] = true
		}

		// Устанавливаем разумные значения по умолчанию для Chassis вентиляторов
		if minRPM == 0 && fanType == "Chassis" && len(activeFansOfType) > 0 {
			minRPM = 1000
		}

		// Создаем требование
		req := FanRequirement{
			Name:           fmt.Sprintf("%s fans (%d active, %d N/A)", fanType, len(activeFansOfType), len(naFansOfType)),
			FanType:        fanType,
			Positions:      positions,
			MinCount:       len(activeFansOfType),
			MinRPM:         minRPM,
			MaxRPM:         maxRPM,
			MaxRPMDiff:     500,
			ExpectedStatus: expectedStatus,
		}

		requirements = append(requirements, req)
	}

	// Предлагаем игнорируемые ряды для визуализации
	ignoreRows := suggestIgnoreRows(rowAnalysis)

	config := Config{
		FanRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:    typeVisuals,
			PositionToSlot: positionToSlot,
			TotalSlots:     len(allFans) + 2,
			SlotWidth:      9,
			IgnoreRows:     ignoreRows,
			MaxRowSlots:    8,
		},
		CheckTargetRPM: false,
		IPMITimeout:    30,
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

	printSuccess("Configuration created successfully using IPMI data")
	printInfo(fmt.Sprintf("Total fan positions mapped: %d", len(positionToSlot)))
	printInfo(fmt.Sprintf("Visual patterns created for %d fan types", len(typeVisuals)))
	printInfo("All data sourced from IPMI sensors")

	if len(ignoreRows) > 0 {
		printInfo("Suggested visualization ignore patterns:")
		for _, pattern := range ignoreRows {
			printInfo(fmt.Sprintf("  - '%s'", pattern))
		}
	}

	return nil
}

func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	// Устанавливаем значения по умолчанию
	if config.Visualization.MaxRowSlots == 0 {
		config.Visualization.MaxRowSlots = 8
	}
	if config.IPMITimeout == 0 {
		config.IPMITimeout = 30
	}

	return &config, nil
}

// === CHECKING FUNCTIONS ===

func checkFanAgainstRequirements(fan FanInfo, requirements []FanRequirement) FanCheckResult {
	result := FanCheckResult{
		Status:   "ok",
		RPMOK:    true,
		StatusOK: true,
	}

	var matchingReqs []FanRequirement

	for _, req := range requirements {
		if req.FanType != "" && req.FanType != "any" && fan.FanType != req.FanType {
			continue
		}

		if len(req.Positions) > 0 {
			found := false
			for _, position := range req.Positions {
				if fan.Position == position {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		matchingReqs = append(matchingReqs, req)
	}

	if len(matchingReqs) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range matchingReqs {
		// Проверяем соответствие текущего статуса ожидаемому (нормализуем оба для сравнения)
		if expectedStatus, exists := req.ExpectedStatus[fan.Position]; exists {
			normalizedExpected := normalizeStatus(expectedStatus)
			normalizedCurrent := normalizeStatus(fan.Status)

			if normalizedCurrent != normalizedExpected {
				if normalizedExpected == "N/A" && normalizedCurrent != "N/A" {
					result.Issues = append(result.Issues, fmt.Sprintf("Fan unexpectedly present (expected N/A, got %s)", normalizedCurrent))
					hasWarnings = true
				} else if normalizedExpected != "N/A" && normalizedCurrent == "N/A" {
					result.Issues = append(result.Issues, fmt.Sprintf("Fan unexpectedly absent (expected %s, got N/A)", normalizedExpected))
					result.StatusOK = false
					hasErrors = true
				} else {
					result.Issues = append(result.Issues, fmt.Sprintf("Status changed (expected %s, got %s)", normalizedExpected, normalizedCurrent))
					hasWarnings = true
				}
			}
		}

		// Пропускаем дальнейшие проверки для N/A вентиляторов, которые и должны быть N/A
		if expectedStatus, exists := req.ExpectedStatus[fan.Position]; exists && normalizeStatus(expectedStatus) == "N/A" && normalizeStatus(fan.Status) == "N/A" {
			continue
		}

		// Проверяем требования для активных вентиляторов
		if fan.Status != "N/A" {
			if req.MinRPM > 0 {
				if fan.CurrentRPM == 0 {
					result.Issues = append(result.Issues, "Fan not spinning (0 RPM)")
					result.RPMOK = false
					hasErrors = true
				} else if fan.CurrentRPM < req.MinRPM {
					result.Issues = append(result.Issues, fmt.Sprintf("RPM too low: %d (required min %d)", fan.CurrentRPM, req.MinRPM))
					result.RPMOK = false
					hasErrors = true
				}
			}

			if req.MaxRPM > 0 && fan.CurrentRPM > req.MaxRPM {
				result.Issues = append(result.Issues, fmt.Sprintf("RPM too high: %d (max %d)", fan.CurrentRPM, req.MaxRPM))
				result.RPMOK = false
				hasErrors = true
			}

			if fan.Status == "FAIL" {
				result.Issues = append(result.Issues, "Fan status: FAIL")
				result.StatusOK = false
				hasErrors = true
			} else if fan.Status == "ALARM" {
				result.Issues = append(result.Issues, "Fan status: ALARM")
				result.StatusWarn = true
				hasWarnings = true
			}

			if req.MaxRPMDiff > 0 && fan.TargetRPM > 0 {
				diff := fan.CurrentRPM - fan.TargetRPM
				if diff < 0 {
					diff = -diff
				}
				if diff > req.MaxRPMDiff {
					result.Issues = append(result.Issues, fmt.Sprintf("RPM deviation: %d RPM from target %d (max diff %d)", diff, fan.TargetRPM, req.MaxRPMDiff))
					result.RPMWarn = true
					hasWarnings = true
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

func checkFans(config *Config) error {
	printInfo("Starting IPMI fan check...")

	fans, err := getFanInfo(config.IPMITimeout)
	if err != nil {
		return fmt.Errorf("failed to get fan info via IPMI: %v", err)
	}

	activeFans := 0
	naFans := 0
	for _, fan := range fans {
		if fan.Status == "N/A" {
			naFans++
		} else {
			activeFans++
		}
	}

	printInfo(fmt.Sprintf("Found total fan positions via IPMI: %d", len(fans)))
	printInfo(fmt.Sprintf("  - Active fans: %d", activeFans))
	printInfo(fmt.Sprintf("  - N/A fans: %d", naFans))

	if len(fans) == 0 {
		printError("No fan positions found via IPMI")
		return fmt.Errorf("no fan positions found")
	}

	// Отображаем найденные вентиляторы
	for i, fan := range fans {
		if fan.Status == "N/A" {
			printInfo(fmt.Sprintf("Fan %d: %s (N/A - physically absent)", i+1, fan.Name))
		} else {
			printInfo(fmt.Sprintf("Fan %d: %s", i+1, fan.Name))
		}
		printDebug(fmt.Sprintf("  Position: %s, Type: %s, RPM: %d, Status: %s, Sensor: %d",
			fan.Position, fan.FanType, fan.CurrentRPM, fan.Status, fan.SensorNumber))
	}

	// Проверяем требования
	allPassed := true
	for _, req := range config.FanRequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		matchingFans := filterFans(fans, req)

		expectedActive := 0
		expectedNA := 0
		actualActive := 0
		actualNA := 0

		for _, expectedStatus := range req.ExpectedStatus {
			if expectedStatus == "N/A" {
				expectedNA++
			} else {
				expectedActive++
			}
		}

		for _, fan := range matchingFans {
			if fan.Status == "N/A" {
				actualNA++
			} else {
				actualActive++
			}
		}

		printInfo(fmt.Sprintf("  Expected: %d active, %d N/A", expectedActive, expectedNA))
		printInfo(fmt.Sprintf("  Found: %d active, %d N/A", actualActive, actualNA))

		if actualActive < req.MinCount {
			printError(fmt.Sprintf("  Requirement FAILED: found %d active fan(s), required %d", actualActive, req.MinCount))
			allPassed = false
			continue
		}

		reqPassed := true
		for i, fan := range matchingFans {
			expectedStatus := req.ExpectedStatus[fan.Position]
			normalizedExpected := normalizeStatus(expectedStatus)
			normalizedCurrent := normalizeStatus(fan.Status)

			if normalizedCurrent == "N/A" && normalizedExpected == "N/A" {
				printSuccess(fmt.Sprintf("    Fan %d: %s (N/A as expected)", i+1, fan.Name))
				continue
			} else if normalizedCurrent == "N/A" && normalizedExpected != "N/A" {
				printError(fmt.Sprintf("    Fan %d: %s - FAILED (expected active, got N/A)", i+1, fan.Name))
				reqPassed = false
				continue
			} else if normalizedCurrent != "N/A" && normalizedExpected == "N/A" {
				printWarning(fmt.Sprintf("    Fan %d: %s - WARNING (expected N/A, but fan is present)", i+1, fan.Name))
				continue
			}

			printInfo(fmt.Sprintf("    Fan %d: %s (Sensor ID: %d)", i+1, fan.Name, fan.SensorNumber))

			if req.MinRPM > 0 {
				if fan.CurrentRPM == 0 {
					printError(fmt.Sprintf("      RPM FAILED: fan not spinning"))
					reqPassed = false
				} else if fan.CurrentRPM < req.MinRPM {
					printError(fmt.Sprintf("      RPM FAILED: %d RPM (required min %d)", fan.CurrentRPM, req.MinRPM))
					reqPassed = false
				} else {
					printSuccess(fmt.Sprintf("      RPM OK: %d RPM", fan.CurrentRPM))
				}
			}

			if req.MaxRPM > 0 && fan.CurrentRPM > req.MaxRPM {
				printError(fmt.Sprintf("      RPM FAILED: %d RPM (max %d)", fan.CurrentRPM, req.MaxRPM))
				reqPassed = false
			}

			if normalizedCurrent == "FAIL" {
				printError(fmt.Sprintf("      Status FAILED: %s", normalizedCurrent))
				reqPassed = false
			} else if normalizedCurrent == "ALARM" {
				printWarning(fmt.Sprintf("      Status WARNING: %s", normalizedCurrent))
			} else {
				printSuccess(fmt.Sprintf("      Status OK: %s", normalizedCurrent))
			}
		}

		if reqPassed {
			printSuccess(fmt.Sprintf("  Requirement PASSED: %s", req.Name))
		} else {
			printError(fmt.Sprintf("  Requirement FAILED: %s", req.Name))
			allPassed = false
		}
	}

	if allPassed {
		printSuccess("All fan requirements passed")
		printSuccess(fmt.Sprintf("IPMI validation successful: %d active fans operating correctly, %d N/A fans as expected", activeFans, naFans))
	} else {
		printError("Some fan requirements failed")
		return fmt.Errorf("fan requirements not met")
	}

	return nil
}

// === VISUALIZATION FUNCTIONS ===

func visualizeSlots(fans []FanInfo, config *Config) error {
	return visualizeSlotsInternal(fans, config, false)
}

func visualizeSlotsMultiRow(fans []FanInfo, config *Config) error {
	return visualizeSlotsInternal(fans, config, true)
}

func visualizeSlotsInternal(fans []FanInfo, config *Config, multiRow bool) error {
	if multiRow {
		printInfo("Fan Layout (Multi-Row) - IPMI Data:")
	} else {
		printInfo("Fan Layout - IPMI Data:")
	}
	fmt.Println()

	// Применяем фильтры игнорирования для полного удаления игнорированных рядов
	allFans := fans
	displayFans := filterFansByIgnorePatterns(fans, config.Visualization.IgnoreRows)

	// Фильтруем также позиции в конфигурации
	filteredPositionToSlot := filterPositionToSlotByIgnorePatterns(config.Visualization.PositionToSlot, config.Visualization.IgnoreRows)

	if len(displayFans) < len(allFans) {
		printInfo(fmt.Sprintf("Applying visualization filters: showing %d of %d fans", len(displayFans), len(allFans)))
		for _, pattern := range config.Visualization.IgnoreRows {
			printInfo(fmt.Sprintf("  - Ignoring rows matching pattern: '%s'", pattern))
		}
	}

	// Вычисляем максимальное количество слотов на основе отфильтрованных позиций
	maxSlots := 0
	for _, slot := range filteredPositionToSlot {
		if slot > maxSlots {
			maxSlots = slot
		}
	}

	if maxSlots == 0 {
		maxSlots = len(displayFans) + 2
	}

	// Создаем массивы слотов
	slotData := make([]FanInfo, maxSlots+1)
	slotResults := make([]FanCheckResult, maxSlots+1)

	// Заполняем слоты отображаемыми вентиляторами
	foundPositions := make(map[string]bool)
	for _, fan := range displayFans {
		foundPositions[fan.Position] = true
		if logicalSlot, exists := filteredPositionToSlot[fan.Position]; exists {
			if logicalSlot > 0 && logicalSlot <= maxSlots {
				slotData[logicalSlot] = fan
				slotResults[logicalSlot] = checkFanAgainstRequirements(fan, config.FanRequirements)
			}
		}
	}

	// Проверяем проблемы
	hasErrors := false
	hasWarnings := false
	missingFans := []string{}
	statusChangedFans := []string{}

	for position, expectedSlot := range filteredPositionToSlot {
		if expectedSlot == 0 {
			continue
		}

		if expectedSlot > 0 && expectedSlot <= maxSlots {
			expectedStatus := getExpectedStatus(position, config.FanRequirements)

			if !foundPositions[position] {
				fanInAll := findFanByPosition(allFans, position)
				if fanInAll != nil {
					if expectedStatus != "" && fanInAll.Status != expectedStatus {
						statusMessage := fmt.Sprintf("%s: expected %s, got %s (filtered from display)", position, expectedStatus, fanInAll.Status)
						statusChangedFans = append(statusChangedFans, statusMessage)
						hasWarnings = true
					}
					continue
				}

				slotResults[expectedSlot] = FanCheckResult{Status: "missing"}
				missingFans = append(missingFans, fmt.Sprintf("%s (slot %d)", position, expectedSlot))
				hasErrors = true

				slotData[expectedSlot] = FanInfo{
					Name:     fmt.Sprintf("MISSING: %s", position),
					Position: position,
				}
			} else {
				currentFan := findFanByPosition(displayFans, position)
				if currentFan != nil && expectedStatus != "" {
					normalizedExpected := normalizeStatus(expectedStatus)
					normalizedCurrent := normalizeStatus(currentFan.Status)

					if normalizedCurrent != normalizedExpected {
						statusMessage := fmt.Sprintf("%s: expected %s, got %s", position, normalizedExpected, normalizedCurrent)
						statusChangedFans = append(statusChangedFans, statusMessage)

						if normalizedExpected == "N/A" && normalizedCurrent != "N/A" {
							hasWarnings = true
						} else if normalizedExpected != "N/A" && normalizedCurrent == "N/A" {
							hasErrors = true
						} else {
							hasWarnings = true
						}
					}
				}
			}
		}
	}

	// Подсчитываем итоговый статус
	for i := 1; i <= maxSlots; i++ {
		status := slotResults[i].Status
		if status == "error" || status == "missing" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Выводим легенду и проблемы
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Working as Expected  ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Status Changed/Issues  ", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Missing/Failed  ", ColorRed, "░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░", ColorReset)
	fmt.Println()

	if len(missingFans) > 0 {
		printError("Missing fan positions:")
		for _, fan := range missingFans {
			printError(fmt.Sprintf("  - %s", fan))
		}
		fmt.Println()
	}

	if len(statusChangedFans) > 0 {
		printWarning("Fan status changes:")
		for _, change := range statusChangedFans {
			printWarning(fmt.Sprintf("  - %s", change))
		}
		fmt.Println()
	}

	// Выбираем метод визуализации
	if multiRow {
		err := renderMultiRowVisualization(slotData, slotResults, config, maxSlots)
		if err != nil {
			return err
		}
	} else {
		err := renderSingleRowVisualization(slotData, slotResults, config, maxSlots)
		if err != nil {
			return err
		}
	}

	// Итоговый статус
	if hasErrors {
		printError("Fan configuration validation FAILED!")
		return fmt.Errorf("fan configuration validation failed")
	} else if hasWarnings {
		printWarning("Fan configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All fans match expected configuration!")
		return nil
	}
}

func renderSingleRowVisualization(slotData []FanInfo, slotResults []FanCheckResult, config *Config, maxSlots int) error {
	width := config.Visualization.SlotWidth

	// Верхняя граница
	fmt.Print("┌")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┬")
		}
	}
	fmt.Println("┐")

	// Ряд символов
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		visual := getFanVisualByType(slotData[i], &config.Visualization)
		result := slotResults[i]
		expectedStatus := getExpectedStatus(slotData[i].Position, config.FanRequirements)

		symbolText := centerText(visual.Symbol, width)

		if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
			fmt.Print(ColorGreen + centerText("░░░", width) + ColorReset)
		} else {
			switch result.Status {
			case "ok":
				fmt.Print(ColorGreen + symbolText + ColorReset)
			case "warning":
				fmt.Print(ColorYellow + symbolText + ColorReset)
			case "missing", "error":
				fmt.Print(ColorRed + centerText("░░░", width) + ColorReset)
			default:
				fmt.Print(symbolText)
			}
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Ряд коротких имен
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		expectedStatus := getExpectedStatus(slotData[i].Position, config.FanRequirements)

		if slotData[i].Name != "" {
			visual := getFanVisualByType(slotData[i], &config.Visualization)
			nameText := centerText(visual.ShortName, width)

			if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				fmt.Print(ColorGreen + centerText("N/A", width) + ColorReset)
			} else {
				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + nameText + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + nameText + ColorReset)
				case "missing", "error":
					fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
				default:
					fmt.Print(nameText)
				}
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Ряд RPM
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		expectedStatus := getExpectedStatus(slotData[i].Position, config.FanRequirements)

		if slotData[i].Name != "" {
			var rpmInfo string
			if result.Status == "missing" {
				rpmInfo = "?"
			} else if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				rpmInfo = "N/A"
			} else {
				rpmInfo = formatRPM(slotData[i].CurrentRPM)
			}

			rpmText := centerText(rpmInfo, width)

			if result.Status == "missing" || result.Status == "error" {
				fmt.Print(ColorRed + rpmText + ColorReset)
			} else if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				fmt.Print(ColorGreen + rpmText + ColorReset)
			} else if !result.RPMOK {
				fmt.Print(ColorRed + rpmText + ColorReset)
			} else if result.Status == "warning" {
				fmt.Print(ColorYellow + rpmText + ColorReset)
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + rpmText + ColorReset)
			} else {
				fmt.Print(rpmText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Ряд статуса
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		expectedStatus := getExpectedStatus(slotData[i].Position, config.FanRequirements)

		if slotData[i].Name != "" {
			var statusInfo string
			if result.Status == "missing" {
				statusInfo = "?"
			} else {
				statusInfo = slotData[i].Status
			}

			statusText := centerText(statusInfo, width)

			if result.Status == "missing" || result.Status == "error" {
				fmt.Print(ColorRed + statusText + ColorReset)
			} else if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				fmt.Print(ColorGreen + statusText + ColorReset)
			} else if !result.StatusOK {
				fmt.Print(ColorRed + statusText + ColorReset)
			} else if result.Status == "warning" {
				fmt.Print(ColorYellow + statusText + ColorReset)
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + statusText + ColorReset)
			} else {
				fmt.Print(statusText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Нижняя граница
	fmt.Print("└")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┴")
		}
	}
	fmt.Println("┘")

	// Подписи
	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(centerText(fmt.Sprintf("%d", i), width+1))
	}
	fmt.Println("(Logic)")

	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		position := ""
		if slotData[i].Name != "" {
			position = slotData[i].Position
		} else {
			position = "-"
		}
		fmt.Print(centerText(position, width+1))
	}
	fmt.Println("(Position)")

	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		sensorID := ""
		if slotData[i].Name != "" {
			sensorID = fmt.Sprintf("%d", slotData[i].SensorNumber)
		} else {
			sensorID = "-"
		}
		fmt.Print(centerText(sensorID, width+1))
	}
	fmt.Println("(Sensor)")

	fmt.Println()

	return nil
}

func renderMultiRowVisualization(slotData []FanInfo, slotResults []FanCheckResult, config *Config, maxSlots int) error {
	width := config.Visualization.SlotWidth
	maxRowSlots := config.Visualization.MaxRowSlots
	if maxRowSlots == 0 {
		maxRowSlots = 8
	}

	totalRows := (maxSlots + maxRowSlots - 1) / maxRowSlots

	for row := 0; row < totalRows; row++ {
		startSlot := row*maxRowSlots + 1
		endSlot := startSlot + maxRowSlots - 1
		if endSlot > maxSlots {
			endSlot = maxSlots
		}
		rowSlots := endSlot - startSlot + 1

		fmt.Printf("Row %d (Slots %d-%d):\n", row+1, startSlot, endSlot)

		// Верхняя граница
		fmt.Print("┌")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Ряд символов
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			visual := getFanVisualByType(slotData[slotIdx], &config.Visualization)
			result := slotResults[slotIdx]
			expectedStatus := getExpectedStatus(slotData[slotIdx].Position, config.FanRequirements)

			symbolText := centerText(visual.Symbol, width)

			if expectedStatus == "N/A" && slotData[slotIdx].Status == "N/A" {
				fmt.Print(ColorGreen + centerText("░░░", width) + ColorReset)
			} else {
				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + symbolText + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + symbolText + ColorReset)
				case "missing", "error":
					fmt.Print(ColorRed + centerText("░░░", width) + ColorReset)
				default:
					fmt.Print(symbolText)
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Ряд коротких имен
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			result := slotResults[slotIdx]
			expectedStatus := getExpectedStatus(slotData[slotIdx].Position, config.FanRequirements)

			if slotData[slotIdx].Name != "" {
				visual := getFanVisualByType(slotData[slotIdx], &config.Visualization)
				nameText := centerText(visual.ShortName, width)

				if expectedStatus == "N/A" && slotData[slotIdx].Status == "N/A" {
					fmt.Print(ColorGreen + centerText("N/A", width) + ColorReset)
				} else {
					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + nameText + ColorReset)
					case "warning":
						fmt.Print(ColorYellow + nameText + ColorReset)
					case "missing", "error":
						fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
					default:
						fmt.Print(nameText)
					}
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Ряд RPM
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			result := slotResults[slotIdx]
			expectedStatus := getExpectedStatus(slotData[slotIdx].Position, config.FanRequirements)

			if slotData[slotIdx].Name != "" {
				var rpmInfo string
				if result.Status == "missing" {
					rpmInfo = "?"
				} else if expectedStatus == "N/A" && slotData[slotIdx].Status == "N/A" {
					rpmInfo = "N/A"
				} else {
					rpmInfo = formatRPM(slotData[slotIdx].CurrentRPM)
				}

				rpmText := centerText(rpmInfo, width)

				if result.Status == "missing" || result.Status == "error" {
					fmt.Print(ColorRed + rpmText + ColorReset)
				} else if expectedStatus == "N/A" && slotData[slotIdx].Status == "N/A" {
					fmt.Print(ColorGreen + rpmText + ColorReset)
				} else if !result.RPMOK {
					fmt.Print(ColorRed + rpmText + ColorReset)
				} else if result.Status == "warning" {
					fmt.Print(ColorYellow + rpmText + ColorReset)
				} else if result.Status == "ok" {
					fmt.Print(ColorGreen + rpmText + ColorReset)
				} else {
					fmt.Print(rpmText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Нижняя граница
		fmt.Print("└")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Подписи
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			fmt.Print(centerText(fmt.Sprintf("%d", slotIdx), width+1))
		}
		fmt.Println("(Logic)")

		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			position := ""
			if slotData[slotIdx].Name != "" {
				position = slotData[slotIdx].Position
			} else {
				position = "-"
			}
			fmt.Print(centerText(position, width+1))
		}
		fmt.Println("(Position)")

		fmt.Println()
	}

	return nil
}

// === MAIN FUNCTION ===

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "fan_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected fans without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual fan layout")
		multiRow     = flag.Bool("multirow", false, "Show visual layout in multiple rows")
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
		printInfo("Scanning for fans via IPMI...")
		fans, err := getFanInfo(30) // В пизду импорт конфига
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information via IPMI: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}

		if len(fans) == 0 {
			printWarning("No fans found via IPMI")
		} else {
			printSuccess(fmt.Sprintf("Found fans via IPMI: %d", len(fans)))
			for i, fan := range fans {
				fmt.Printf("\nFan %d:\n", i+1)
				fmt.Printf("  Name: %s\n", fan.Name)
				fmt.Printf("  Position: %s\n", fan.Position)
				fmt.Printf("  Type: %s\n", fan.FanType)
				fmt.Printf("  Current RPM: %d\n", fan.CurrentRPM)
				fmt.Printf("  Min RPM: %d\n", fan.MinRPM)
				fmt.Printf("  Max RPM: %d\n", fan.MaxRPM)
				fmt.Printf("  Status: %s\n", fan.Status)
				fmt.Printf("  Sensor Number: %d\n", fan.SensorNumber)
				fmt.Printf("  Raw Value: %.2f\n", fan.RawValue)
				fmt.Printf("  Units: %s\n", fan.Units)
			}
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

	if *visualize || *multiRow {
		printInfo("Scanning for fans via IPMI...")
		fans, err := getFanInfo(30) // И здесь тоже в пизду
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information via IPMI: %v", err))
			printInfo("Try running with -test flag to diagnose IPMI issues")
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		if *multiRow {
			err = visualizeSlotsMultiRow(fans, config)
		} else {
			err = visualizeSlots(fans, config)
		}
		if err != nil {
			os.Exit(1)
		}
		return
	}

	// По умолчанию: загружаем конфигурацию и выполняем проверку вентиляторов
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found fans")
		printInfo("Use -test to diagnose IPMI connectivity issues")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkFans(config)
	if err != nil {
		printError(fmt.Sprintf("Fan check failed: %v", err))
		printInfo("Try running with -test flag to diagnose IPMI issues")
		os.Exit(1)
	}
}
