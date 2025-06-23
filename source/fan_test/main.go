package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bougou/go-ipmi"
)

const VERSION = "1.1.0"

type FanInfo struct {
	Name       string `json:"name"`
	Position   string `json:"position"` // CPU1, CHS1, PSU1, FAN1_1, etc.
	CurrentRPM int    `json:"current_rpm"`
	TargetRPM  int    `json:"target_rpm"`  // если доступно
	MinRPM     int    `json:"min_rpm"`     // если доступно
	MaxRPM     int    `json:"max_rpm"`     // если доступно
	Status     string `json:"status"`      // OK, FAIL, ALARM, N/A, etc.
	PWMPercent int    `json:"pwm_percent"` // если доступно
	Sensor     string `json:"sensor"`      // источник данных (chip название)
	FanType    string `json:"fan_type"`    // CPU, Chassis, PSU, PCIe, etc.
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
	ColorGreen  = "\033[32m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
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
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

// === FAN DETECTION METHODS ===

func getFanInfo() ([]FanInfo, error) {
	type result struct {
		fans []FanInfo
		err  error
	}
	ch := make(chan result, 3)

	// Launch parallel detection methods
	go func() { fans, err := getFanInfoFromSensors(); ch <- result{fans, err} }()
	go func() { fans, err := getFanInfoFromHwmon(); ch <- result{fans, err} }()
	go func() { fans, err := getFanInfoFromIPMI(); ch <- result{fans, err} }()

	var errs []error
	for i := 0; i < 3; i++ {
		res := <-ch
		if res.err == nil && len(res.fans) > 0 {
			return res.fans, nil
		}
		if res.err != nil {
			errs = append(errs, res.err)
		}
	}

	var sb strings.Builder
	sb.WriteString("no fans found using any detection method:")
	for _, e := range errs {
		sb.WriteString(" ")
		sb.WriteString(e.Error())
		sb.WriteString(";")
	}
	return nil, fmt.Errorf("%s", sb.String())
}

func getFanInfoFromSensors() ([]FanInfo, error) {
	// Check if sensors command exists
	if _, err := exec.LookPath("sensors"); err != nil {
		return nil, fmt.Errorf("sensors command not found")
	}

	// Run sensors command
	cmd := exec.Command("sensors", "-A")
	output, err := cmd.Output()
	if err != nil {
		// Try without -A flag as fallback
		cmd = exec.Command("sensors")
		output, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to run sensors command: %v", err)
		}
	}

	outputStr := string(output)
	if debugMode {
		printDebug("sensors command output:")
		printDebug(outputStr)
	}

	return parseSensorsOutput(outputStr), nil
}

func getFanInfoFromHwmon() ([]FanInfo, error) {
	var fans []FanInfo

	hwmonPath := "/sys/class/hwmon"
	entries, err := ioutil.ReadDir(hwmonPath)
	if err != nil {
		return nil, fmt.Errorf("cannot access %s: %v", hwmonPath, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		hwmonDir := filepath.Join(hwmonPath, entry.Name())

		// Read name file if available
		nameFile := filepath.Join(hwmonDir, "name")
		chipName := "unknown"
		if nameData, err := ioutil.ReadFile(nameFile); err == nil {
			chipName = strings.TrimSpace(string(nameData))
		}

		// Look for fan input files
		files, err := ioutil.ReadDir(hwmonDir)
		if err != nil {
			continue
		}

		for _, file := range files {
			if strings.HasPrefix(file.Name(), "fan") && strings.HasSuffix(file.Name(), "_input") {
				// Extract fan number
				fanNum := strings.TrimPrefix(file.Name(), "fan")
				fanNum = strings.TrimSuffix(fanNum, "_input")

				// Read current RPM
				rpmFile := filepath.Join(hwmonDir, file.Name())
				rpmData, err := ioutil.ReadFile(rpmFile)
				if err != nil {
					continue
				}

				rpm, err := strconv.Atoi(strings.TrimSpace(string(rpmData)))
				if err != nil {
					continue
				}

				// Try to read min/max values
				minRPM := 0
				maxRPM := 0

				minFile := filepath.Join(hwmonDir, fmt.Sprintf("fan%s_min", fanNum))
				if minData, err := ioutil.ReadFile(minFile); err == nil {
					if min, err := strconv.Atoi(strings.TrimSpace(string(minData))); err == nil {
						minRPM = min
					}
				}

				maxFile := filepath.Join(hwmonDir, fmt.Sprintf("fan%s_max", fanNum))
				if maxData, err := ioutil.ReadFile(maxFile); err == nil {
					if max, err := strconv.Atoi(strings.TrimSpace(string(maxData))); err == nil {
						maxRPM = max
					}
				}

				// Create fan info
				fanName := fmt.Sprintf("fan%s", fanNum)
				fan := FanInfo{
					Name:       fanName,
					Position:   normalizeFanPosition(fanName),
					CurrentRPM: rpm,
					MinRPM:     minRPM,
					MaxRPM:     maxRPM,
					Sensor:     chipName,
					FanType:    determineFanType(fanName),
				}

				// Determine status
				if rpm == 0 {
					fan.Status = "FAIL"
				} else if minRPM > 0 && rpm < minRPM {
					fan.Status = "ALARM"
				} else {
					fan.Status = "OK"
				}

				fans = append(fans, fan)
			}
		}
	}

	return fans, nil
}

func getFanInfoFromIPMI() ([]FanInfo, error) {
	client, err := ipmi.NewOpenClient()
	if err != nil {
		return nil, fmt.Errorf("new IPMI client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect to BMC: %v", err)
	}
	defer client.Close(ctx)

	sdrs, err := client.GetSDRs(ctx,
		ipmi.SDRRecordTypeFullSensor,
		ipmi.SDRRecordTypeCompactSensor,
	)
	if err != nil {
		return nil, fmt.Errorf("GetSDRs failed: %w", err)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		fans     []FanInfo
		firstErr error
	)

	// Iterate SDRs and fire goroutine per fan
	for _, sdr := range sdrs {
		// Only FullSensor records can be analog fans
		if sdr.Full == nil {
			continue
		}
		// Must have analog reading and be a FAN type
		if !sdr.HasAnalogReading() || sdr.Full.SensorType != ipmi.SensorTypeFan {
			continue
		}

		name := sdr.SensorName()
		num := uint8(sdr.SensorNumber())

		wg.Add(1)
		go func(name string, num uint8) {
			defer wg.Done()
			resp, err := client.GetSensorByID(ctx, num)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			fans = append(fans, FanInfo{
				Name:       name,
				Position:   normalizeIPMIFanPosition(name),
				CurrentRPM: int(resp.Value),
				Status:     resp.Status(),
				Sensor:     "IPMI",
				FanType:    determineIPMIFanType(name),
				MinRPM:     int(resp.Threshold.LNC),
				MaxRPM:     int(resp.Threshold.UNC),
			})
			mu.Unlock()
		}(name, num)
	}

	wg.Wait()
	if len(fans) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("no fans found via IPMI: %v", firstErr)
		}
		return nil, fmt.Errorf("no fan sensors detected via IPMI")
	}
	return fans, nil
}

// === PARSING FUNCTIONS ===

func parseSensorsOutput(output string) []FanInfo {
	var fans []FanInfo

	sections := strings.Split(output, "\n\n")

	for _, section := range sections {
		if strings.TrimSpace(section) == "" {
			continue
		}

		lines := strings.Split(section, "\n")
		if len(lines) == 0 {
			continue
		}

		chipName := strings.TrimSpace(lines[0])

		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			lineLower := strings.ToLower(line)
			if (strings.Contains(lineLower, "fan") && strings.Contains(line, "RPM")) ||
				(strings.Contains(lineLower, "fan") && strings.Contains(line, "rpm")) {

				fan := parseFanLine(line, chipName)
				if fan.Name != "" {
					fans = append(fans, fan)
				}
			}
		}
	}

	return fans
}

func parseFanLine(line, chipName string) FanInfo {
	var fan FanInfo
	fan.Sensor = chipName

	colonIndex := strings.Index(line, ":")
	if colonIndex == -1 {
		return fan
	}

	fanName := strings.TrimSpace(line[:colonIndex])
	rest := strings.TrimSpace(line[colonIndex+1:])

	fan.Name = fanName
	fan.Position = normalizeFanPosition(fanName)
	fan.FanType = determineFanType(fanName)

	// Parse RPM values
	rpmRegex := regexp.MustCompile(`(?i)(\d+)\s*RPM`)
	matches := rpmRegex.FindAllStringSubmatch(rest, -1)

	if len(matches) > 0 {
		if rpm, err := strconv.Atoi(matches[0][1]); err == nil {
			fan.CurrentRPM = rpm
		}
	}

	// Parse min RPM
	minRegex := regexp.MustCompile(`(?i)min\s*=\s*(\d+)\s*RPM`)
	if matches := minRegex.FindStringSubmatch(rest); len(matches) > 1 {
		if minRPM, err := strconv.Atoi(matches[1]); err == nil {
			fan.MinRPM = minRPM
		}
	}

	// Parse max RPM
	maxRegex := regexp.MustCompile(`(?i)max\s*=\s*(\d+)\s*RPM`)
	if matches := maxRegex.FindStringSubmatch(rest); len(matches) > 1 {
		if maxRPM, err := strconv.Atoi(matches[1]); err == nil {
			fan.MaxRPM = maxRPM
		}
	}

	// Check for status indicators
	restUpper := strings.ToUpper(rest)
	if strings.Contains(restUpper, "ALARM") {
		fan.Status = "ALARM"
	} else if strings.Contains(restUpper, "FAIL") {
		fan.Status = "FAIL"
	} else if fan.CurrentRPM == 0 {
		fan.Status = "FAIL"
	} else if fan.MinRPM > 0 && fan.CurrentRPM < fan.MinRPM {
		fan.Status = "ALARM"
	} else {
		fan.Status = "OK"
	}

	return fan
}

// === HELPER FUNCTIONS ===

func normalizeFanPosition(fanName string) string {
	name := strings.ToLower(fanName)
	name = strings.ReplaceAll(name, " ", "")

	if strings.Contains(name, "cpu") {
		if strings.Contains(name, "1") || strings.Contains(name, "fan1") {
			return "CPU1"
		} else if strings.Contains(name, "2") || strings.Contains(name, "fan2") {
			return "CPU2"
		}
		return "CPU1"
	}

	if strings.Contains(name, "chassis") || strings.Contains(name, "case") || strings.Contains(name, "sys") {
		numRegex := regexp.MustCompile(`(\d+)`)
		if matches := numRegex.FindStringSubmatch(name); len(matches) > 1 {
			return fmt.Sprintf("CHS%s", matches[1])
		}
		return "CHS1"
	}

	if strings.Contains(name, "psu") || strings.Contains(name, "power") {
		return "PSU1"
	}

	if strings.HasPrefix(name, "fan") {
		numRegex := regexp.MustCompile(`fan(\d+)`)
		if matches := numRegex.FindStringSubmatch(name); len(matches) > 1 {
			return fmt.Sprintf("FAN%s", matches[1])
		}
	}

	return strings.ToUpper(fanName)
}

func normalizeIPMIFanPosition(fanName string) string {
	name := strings.ToUpper(fanName)
	name = strings.ReplaceAll(name, " ", "")

	if strings.HasPrefix(name, "FAN") {
		numRegex := regexp.MustCompile(`FAN(\d+)(?:_(\d+))?`)
		if matches := numRegex.FindStringSubmatch(name); len(matches) >= 2 {
			fanNum := matches[1]
			if len(matches) > 2 && matches[2] != "" {
				return fmt.Sprintf("FAN%s_%s", fanNum, matches[2])
			} else {
				return fmt.Sprintf("FAN%s", fanNum)
			}
		}
	}

	if strings.Contains(name, "CPU") {
		if strings.Contains(name, "1") {
			return "CPU1"
		} else if strings.Contains(name, "2") {
			return "CPU2"
		}
		return "CPU1"
	}

	if strings.Contains(name, "PSU") {
		if strings.Contains(name, "1") {
			return "PSU1"
		} else if strings.Contains(name, "2") {
			return "PSU2"
		}
		return "PSU1"
	}

	return name
}

func determineFanType(fanName string) string {
	name := strings.ToLower(fanName)

	if strings.Contains(name, "cpu") {
		return "CPU"
	} else if strings.Contains(name, "chassis") || strings.Contains(name, "case") || strings.Contains(name, "sys") {
		return "Chassis"
	} else if strings.Contains(name, "psu") || strings.Contains(name, "power") {
		return "PSU"
	} else if strings.Contains(name, "pci") || strings.Contains(name, "gpu") {
		return "PCIe"
	}

	return "Other"
}

func determineIPMIFanType(fanName string) string {
	name := strings.ToUpper(fanName)

	if strings.Contains(name, "CPU") {
		return "CPU"
	} else if strings.Contains(name, "PSU") || strings.Contains(name, "POWER") {
		return "PSU"
	} else if strings.Contains(name, "CHASSIS") || strings.Contains(name, "SYS") {
		return "Chassis"
	} else if strings.HasPrefix(name, "FAN") {
		return "Chassis"
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

// Новая функция для фильтрации позиций в конфигурации
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
	printInfo("Scanning system for fans to create configuration...")

	allFans, err := getFanInfo()
	if err != nil {
		return fmt.Errorf("could not scan fans: %v", err)
	}

	if len(allFans) == 0 {
		printError("No fans found in system")
		return fmt.Errorf("no fans found - cannot create configuration")
	}

	// Count active vs N/A fans (include ALL fans in configuration)
	activeFans := []FanInfo{}
	naFans := []FanInfo{}

	for _, fan := range allFans {
		if fan.Status == "N/A" {
			naFans = append(naFans, fan)
		} else {
			activeFans = append(activeFans, fan)
		}
	}

	printInfo(fmt.Sprintf("Found %d total fan positions:", len(allFans)))
	printInfo(fmt.Sprintf("  - %d active fans", len(activeFans)))
	printInfo(fmt.Sprintf("  - %d physically absent (N/A) fans", len(naFans)))

	// Analyze fan patterns for ignore suggestions
	rowAnalysis := analyzeFanRows(allFans)

	var requirements []FanRequirement
	typeVisuals := make(map[string]FanVisual)
	positionToSlot := make(map[string]int)

	// Create position to slot mapping for ALL fans (no filtering at this stage)
	for i, fan := range allFans {
		logicalSlot := i + 1
		positionToSlot[fan.Position] = logicalSlot
		printInfo(fmt.Sprintf("  Mapping %s -> Slot %d (Status: %s)", fan.Position, logicalSlot, fan.Status))
	}

	// Group fans by type
	typeGroups := make(map[string][]FanInfo)
	for _, fan := range allFans {
		typeGroups[fan.FanType] = append(typeGroups[fan.FanType], fan)
	}

	// Create requirements and visuals by type
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
			expectedStatus[fan.Position] = fan.Status

			if fan.Status != "N/A" {
				printInfo(fmt.Sprintf("    - %s: %d RPM", fan.Name, fan.CurrentRPM))
				if fan.MinRPM > 0 && (minRPM == 0 || fan.MinRPM < minRPM) {
					minRPM = fan.MinRPM
				}
				if fan.MaxRPM > maxRPM {
					maxRPM = fan.MaxRPM
				}
			} else {
				printInfo(fmt.Sprintf("    - %s: N/A (physically absent)", fan.Name))
			}
		}

		// Create visual pattern for this type (once per type)
		if !createdVisuals[fanType] {
			visual := generateFanVisualByType(fanType)
			typeVisuals[fanType] = visual
			createdVisuals[fanType] = true
		}

		// Set reasonable defaults for chassis fans if needed
		if minRPM == 0 && fanType == "Chassis" && len(activeFansOfType) > 0 {
			minRPM = 1000
		}

		// Create requirement
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

	// Suggest ignore rows for visualization
	ignoreRows := suggestIgnoreRows(rowAnalysis)

	config := Config{
		FanRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:    typeVisuals,
			PositionToSlot: positionToSlot,
			TotalSlots:     len(allFans) + 2,
			SlotWidth:      9,
			IgnoreRows:     ignoreRows,
			MaxRowSlots:    8, // По умолчанию максимум 8 слотов в ряду
		},
		CheckTargetRPM: false,
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0755)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(configPath, data, 0644)
	if err != nil {
		return err
	}

	printSuccess("Configuration created successfully")
	printInfo(fmt.Sprintf("Total fan positions mapped: %d", len(positionToSlot)))
	printInfo(fmt.Sprintf("Visual patterns created for %d fan types", len(typeVisuals)))

	if len(ignoreRows) > 0 {
		printInfo("Suggested visualization ignore patterns:")
		for _, pattern := range ignoreRows {
			printInfo(fmt.Sprintf("  - '%s'", pattern))
		}
	}

	return nil
}

func loadConfig(configPath string) (*Config, error) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	// Set default values if not specified
	if config.Visualization.MaxRowSlots == 0 {
		config.Visualization.MaxRowSlots = 8
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
		// Check if current status matches expected status
		if expectedStatus, exists := req.ExpectedStatus[fan.Position]; exists {
			if fan.Status != expectedStatus {
				if expectedStatus == "N/A" && fan.Status != "N/A" {
					result.Issues = append(result.Issues, fmt.Sprintf("Fan unexpectedly present (expected N/A, got %s)", fan.Status))
					hasWarnings = true
				} else if expectedStatus != "N/A" && fan.Status == "N/A" {
					result.Issues = append(result.Issues, fmt.Sprintf("Fan unexpectedly absent (expected %s, got N/A)", expectedStatus))
					result.StatusOK = false
					hasErrors = true
				} else {
					result.Issues = append(result.Issues, fmt.Sprintf("Status changed (expected %s, got %s)", expectedStatus, fan.Status))
					hasWarnings = true
				}
			}
		}

		// Skip further checks for N/A fans that are expected to be N/A
		if expectedStatus, exists := req.ExpectedStatus[fan.Position]; exists && expectedStatus == "N/A" && fan.Status == "N/A" {
			continue
		}

		// Check requirements for active fans
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
	printInfo("Starting fan check...")

	fans, err := getFanInfo()
	if err != nil {
		return fmt.Errorf("failed to get fan info: %v", err)
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

	printInfo(fmt.Sprintf("Found total fan positions: %d", len(fans)))
	printInfo(fmt.Sprintf("  - Active fans: %d", activeFans))
	printInfo(fmt.Sprintf("  - N/A fans: %d", naFans))

	if len(fans) == 0 {
		printError("No fan positions found")
		return fmt.Errorf("no fan positions found")
	}

	// Display found fans
	for i, fan := range fans {
		if fan.Status == "N/A" {
			printInfo(fmt.Sprintf("Fan %d: %s (N/A - physically absent)", i+1, fan.Name))
		} else {
			printInfo(fmt.Sprintf("Fan %d: %s", i+1, fan.Name))
		}
		printDebug(fmt.Sprintf("  Position: %s, Type: %s, RPM: %d, Status: %s", fan.Position, fan.FanType, fan.CurrentRPM, fan.Status))
	}

	// Check requirements
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

			if fan.Status == "N/A" && expectedStatus == "N/A" {
				printSuccess(fmt.Sprintf("    Fan %d: %s (N/A as expected)", i+1, fan.Name))
				continue
			} else if fan.Status == "N/A" && expectedStatus != "N/A" {
				printError(fmt.Sprintf("    Fan %d: %s - FAILED (expected active, got N/A)", i+1, fan.Name))
				reqPassed = false
				continue
			} else if fan.Status != "N/A" && expectedStatus == "N/A" {
				printWarning(fmt.Sprintf("    Fan %d: %s - WARNING (expected N/A, but fan is present)", i+1, fan.Name))
				continue
			}

			printInfo(fmt.Sprintf("    Fan %d: %s", i+1, fan.Name))

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

			if fan.Status == "FAIL" {
				printError(fmt.Sprintf("      Status FAILED: %s", fan.Status))
				reqPassed = false
			} else if fan.Status == "ALARM" {
				printWarning(fmt.Sprintf("      Status WARNING: %s", fan.Status))
			} else {
				printSuccess(fmt.Sprintf("      Status OK: %s", fan.Status))
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
		printSuccess(fmt.Sprintf("Configuration validation successful: %d active fans operating correctly, %d N/A fans as expected", activeFans, naFans))
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
		printInfo("Fan Layout (Multi-Row):")
	} else {
		printInfo("Fan Layout:")
	}
	fmt.Println()

	// Apply ignore patterns для полного удаления игнорированных рядов
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

	// Calculate max slots based on filtered positions
	maxSlots := 0
	for _, slot := range filteredPositionToSlot {
		if slot > maxSlots {
			maxSlots = slot
		}
	}

	if maxSlots == 0 {
		maxSlots = len(displayFans) + 2
	}

	// Create slot arrays
	slotData := make([]FanInfo, maxSlots+1)
	slotResults := make([]FanCheckResult, maxSlots+1)

	// Fill slots with display fans
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

	// Check for issues
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
				if currentFan != nil && expectedStatus != "" && currentFan.Status != expectedStatus {
					statusMessage := fmt.Sprintf("%s: expected %s, got %s", position, expectedStatus, currentFan.Status)
					statusChangedFans = append(statusChangedFans, statusMessage)

					if expectedStatus == "N/A" && currentFan.Status != "N/A" {
						hasWarnings = true
					} else if expectedStatus != "N/A" && currentFan.Status == "N/A" {
						hasErrors = true
					} else {
						hasWarnings = true
					}
				}
			}
		}
	}

	// Count final status
	for i := 1; i <= maxSlots; i++ {
		status := slotResults[i].Status
		if status == "error" || status == "missing" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Print legend and issues
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

	// Choose visualization method
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

	// Final status
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

	// Top border
	fmt.Print("┌")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┬")
		}
	}
	fmt.Println("┐")

	// Symbols row
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
			case "warning", "error":
				fmt.Print(ColorYellow + symbolText + ColorReset)
			case "missing":
				fmt.Print(ColorRed + centerText("░░░", width) + ColorReset)
			default:
				fmt.Print(symbolText)
			}
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Short names row
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
				case "warning", "error":
					fmt.Print(ColorYellow + nameText + ColorReset)
				case "missing":
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

	// RPM row
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

			if result.Status == "missing" {
				fmt.Print(ColorRed + rpmText + ColorReset)
			} else if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				fmt.Print(ColorGreen + rpmText + ColorReset)
			} else if !result.RPMOK {
				fmt.Print(ColorRed + rpmText + ColorReset)
			} else if result.Status == "warning" || result.Status == "error" {
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

	// Status row
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

			if result.Status == "missing" {
				fmt.Print(ColorRed + statusText + ColorReset)
			} else if expectedStatus == "N/A" && slotData[i].Status == "N/A" {
				fmt.Print(ColorGreen + statusText + ColorReset)
			} else if !result.StatusOK {
				fmt.Print(ColorRed + statusText + ColorReset)
			} else if result.Status == "warning" || result.Status == "error" {
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

	// Bottom border
	fmt.Print("└")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┴")
		}
	}
	fmt.Println("┘")

	// Labels
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

		// Top border
		fmt.Print("┌")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Symbols row
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
				case "warning", "error":
					fmt.Print(ColorYellow + symbolText + ColorReset)
				case "missing":
					fmt.Print(ColorRed + centerText("░░░", width) + ColorReset)
				default:
					fmt.Print(symbolText)
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Short names row
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
					case "warning", "error":
						fmt.Print(ColorYellow + nameText + ColorReset)
					case "missing":
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

		// RPM row
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

				if result.Status == "missing" {
					fmt.Print(ColorRed + rpmText + ColorReset)
				} else if expectedStatus == "N/A" && slotData[slotIdx].Status == "N/A" {
					fmt.Print(ColorGreen + rpmText + ColorReset)
				} else if !result.RPMOK {
					fmt.Print(ColorRed + rpmText + ColorReset)
				} else if result.Status == "warning" || result.Status == "error" {
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

		// Bottom border
		fmt.Print("└")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Labels
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

	if *listOnly {
		printInfo("Scanning for fans...")
		fans, err := getFanInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information: %v", err))
			os.Exit(1)
		}

		if len(fans) == 0 {
			printWarning("No fans found")
		} else {
			printSuccess(fmt.Sprintf("Found fans: %d", len(fans)))
			for i, fan := range fans {
				fmt.Printf("\nFan %d:\n", i+1)
				fmt.Printf("  Name: %s\n", fan.Name)
				fmt.Printf("  Position: %s\n", fan.Position)
				fmt.Printf("  Type: %s\n", fan.FanType)
				fmt.Printf("  Current RPM: %d\n", fan.CurrentRPM)
				fmt.Printf("  Min RPM: %d\n", fan.MinRPM)
				fmt.Printf("  Max RPM: %d\n", fan.MaxRPM)
				fmt.Printf("  Status: %s\n", fan.Status)
				fmt.Printf("  Sensor: %s\n", fan.Sensor)
			}
		}
		return
	}

	if *createConfig {
		printInfo(fmt.Sprintf("Creating configuration file: %s", *configPath))
		err := createDefaultConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error creating configuration: %v", err))
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully")
		return
	}

	if *visualize || *multiRow {
		printInfo("Scanning for fans...")
		fans, err := getFanInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information: %v", err))
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

	// Default: load configuration and perform fan check
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found fans")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkFans(config)
	if err != nil {
		printError(fmt.Sprintf("Fan check failed: %v", err))
		os.Exit(1)
	}
}
