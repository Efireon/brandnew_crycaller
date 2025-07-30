package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/u-root/u-root/pkg/ipmi"
)

const VERSION = "3.0.1"
const DEFAULT_IPMI_RETRIES = 3
const DEFAULT_REDFISH_RETRIES = 2

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
	RawValue     float64 `json:"raw_value"` // сырое значение
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

type RowConfig struct {
	Name  string `json:"name"`  // Display name for the row
	Slots string `json:"slots"` // Slot range (e.g., "1-4", "5-8")
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Enable custom row configuration
	Rows    []RowConfig `json:"rows"`    // Custom row definitions
}

type VisualizationConfig struct {
	TypeVisuals    map[string]FanVisual `json:"type_visuals"`     // fan_type -> visual (CPU, Chassis, PSU, etc.)
	PositionToSlot map[string]int       `json:"position_to_slot"` // position -> logical slot number
	TotalSlots     int                  `json:"total_slots"`
	SlotWidth      int                  `json:"slot_width"`
	IgnoreRows     []string             `json:"ignore_rows"`   // patterns to ignore during visualization, e.g. ["_2"]
	MaxRowSlots    int                  `json:"max_row_slots"` // максимальное количество слотов в одном ряду для многорядной визуализации
	SlotsPerRow    int                  `json:"slots_per_row"` // Number of slots per row (legacy)
	CustomRows     CustomRowsConfig     `json:"custom_rows"`   // Custom row configuration
}

type RedfishAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Config struct {
	FanRequirements []FanRequirement    `json:"fan_requirements"`
	Visualization   VisualizationConfig `json:"visualization"`
	CheckTargetRPM  bool                `json:"check_target_rpm"`
	RedfishTimeout  int                 `json:"redfish_timeout_seconds"` // таймаут для Redfish операций
	RedfishAuth     RedfishAuth         `json:"redfish_auth"`            // учетные данные для Redfish
}

type FanCheckResult struct {
	Status     string // "ok", "warning", "error", "missing"
	Issues     []string
	RPMOK      bool
	StatusOK   bool
	RPMWarn    bool
	StatusWarn bool
}

// Redfish API structures
type RedfishRoot struct {
	Chassis struct {
		ODataID string `json:"@odata.id"`
	} `json:"Chassis"`
}

type RedfishChassisCollection struct {
	Members []struct {
		ODataID string `json:"@odata.id"`
	} `json:"Members"`
}

type RedfishChassis struct {
	Thermal struct {
		ODataID string `json:"@odata.id"`
	} `json:"Thermal"`
}

type RedfishThermal struct {
	Fans []struct {
		MemberID     string `json:"MemberId"`
		Name         string `json:"Name"`
		Reading      int    `json:"Reading"`
		ReadingUnits string `json:"ReadingUnits"`
		Status       struct {
			State  string `json:"State"`
			Health string `json:"Health"`
		} `json:"Status"`
		LowerThresholdCritical int    `json:"LowerThresholdCritical,omitempty"`
		UpperThresholdCritical int    `json:"UpperThresholdCritical,omitempty"`
		PhysicalContext        string `json:"PhysicalContext,omitempty"`
	} `json:"Fans"`
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
	debugMode   bool
	bmcIPCache  string
	bmcIPMutex  sync.Mutex
	httpClient  *http.Client
	currentAuth RedfishAuth
)

func init() {
	// Настраиваем HTTP клиент с отключенной проверкой SSL
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

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
	fmt.Printf("Fan Checker %s (Redfish Version)\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected fans without configuration check")
	fmt.Println("  -vis        Show visual fan layout")
	fmt.Println("  -multirow   Show visual layout in multiple rows")
	fmt.Println("  -test       Test Redfish connection and show basic info")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -u <login:password>  Set Redfish authentication credentials")
	fmt.Println("  -h          Show this help")
	fmt.Println()
	fmt.Printf("Note: IPMI operations use %d retries with fallback to ipmitool\n", DEFAULT_IPMI_RETRIES)
	fmt.Printf("      Redfish operations use %d retries\n", DEFAULT_REDFISH_RETRIES)
}

// === BMC IP DETECTION VIA IPMI ===

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

// === REDFISH API CLIENT ===

func makeRedfishRequest(bmcIP, path string) ([]byte, error) {
	url := fmt.Sprintf("https://%s%s", bmcIP, path)
	printDebug(fmt.Sprintf("Making Redfish request to: %s", url))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Используем настроенные учетные данные
	req.SetBasicAuth(currentAuth.Username, currentAuth.Password)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	return body, nil
}

// === FAN DETECTION USING REDFISH API ===

func getFanInfo() ([]FanInfo, error) {
	printDebug("Starting Redfish fan detection...")

	bmcIP, err := getBMCIPAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get BMC IP: %v", err)
	}

	// Получаем root service document
	rootData, err := makeRedfishRequest(bmcIP, "/redfish/v1/")
	if err != nil {
		return nil, fmt.Errorf("failed to get Redfish root: %v", err)
	}

	var root RedfishRoot
	if err := json.Unmarshal(rootData, &root); err != nil {
		return nil, fmt.Errorf("failed to parse root response: %v", err)
	}

	// Получаем коллекцию chassis
	chassisData, err := makeRedfishRequest(bmcIP, root.Chassis.ODataID)
	if err != nil {
		return nil, fmt.Errorf("failed to get chassis collection: %v", err)
	}

	var chassisCollection RedfishChassisCollection
	if err := json.Unmarshal(chassisData, &chassisCollection); err != nil {
		return nil, fmt.Errorf("failed to parse chassis collection: %v", err)
	}

	var allFans []FanInfo

	// Проходим по всем chassis и получаем thermal информацию
	for i, chassis := range chassisCollection.Members {
		printDebug(fmt.Sprintf("Processing chassis %d: %s", i+1, chassis.ODataID))

		// Получаем информацию о chassis
		chassisData, err := makeRedfishRequest(bmcIP, chassis.ODataID)
		if err != nil {
			printWarning(fmt.Sprintf("Failed to get chassis info: %v", err))
			continue
		}

		var chassisInfo RedfishChassis
		if err := json.Unmarshal(chassisData, &chassisInfo); err != nil {
			printWarning(fmt.Sprintf("Failed to parse chassis info: %v", err))
			continue
		}

		// Получаем thermal информацию
		if chassisInfo.Thermal.ODataID == "" {
			printWarning("No thermal information available for this chassis")
			continue
		}

		thermalData, err := makeRedfishRequest(bmcIP, chassisInfo.Thermal.ODataID)
		if err != nil {
			printWarning(fmt.Sprintf("Failed to get thermal info: %v", err))
			continue
		}

		var thermal RedfishThermal
		if err := json.Unmarshal(thermalData, &thermal); err != nil {
			printWarning(fmt.Sprintf("Failed to parse thermal info: %v", err))
			continue
		}

		// Обрабатываем каждый вентилятор
		for j, fan := range thermal.Fans {
			fanInfo := convertRedfishFan(fan, j+1)
			allFans = append(allFans, fanInfo)
			printDebug(fmt.Sprintf("Found fan: %s, RPM: %d, Status: %s",
				fanInfo.Name, fanInfo.CurrentRPM, fanInfo.Status))
		}
	}

	if len(allFans) == 0 {
		return nil, fmt.Errorf("no fans found via Redfish API")
	}

	printDebug(fmt.Sprintf("Collected %d fans via Redfish API", len(allFans)))
	return allFans, nil
}

func getFanInfoWithRetry(maxRetries int) ([]FanInfo, error) {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		printDebug(fmt.Sprintf("Fan info retrieval attempt %d/%d", attempt, maxRetries))

		fans, err := getFanInfo()
		if err != nil {
			lastErr = err
			printDebug(fmt.Sprintf("Attempt %d failed: %v", attempt, err))
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
			continue
		}

		return fans, nil
	}

	return nil, fmt.Errorf("failed after %d attempts: %v", maxRetries, lastErr)
}

func convertRedfishFan(redfishFan interface{}, fanIndex int) FanInfo {
	// Type assertion для работы с данными вентилятора
	fan, ok := redfishFan.(struct {
		MemberID     string `json:"MemberId"`
		Name         string `json:"Name"`
		Reading      int    `json:"Reading"`
		ReadingUnits string `json:"ReadingUnits"`
		Status       struct {
			State  string `json:"State"`
			Health string `json:"Health"`
		} `json:"Status"`
		LowerThresholdCritical int    `json:"LowerThresholdCritical,omitempty"`
		UpperThresholdCritical int    `json:"UpperThresholdCritical,omitempty"`
		PhysicalContext        string `json:"PhysicalContext,omitempty"`
	})

	if !ok {
		// Fallback для случая когда структура не совпадает
		return FanInfo{
			Name:         fmt.Sprintf("Unknown Fan %d", fanIndex),
			Position:     fmt.Sprintf("FAN%d", fanIndex),
			FanType:      "Other",
			Status:       "UNKNOWN",
			Units:        "RPM",
			SensorNumber: uint8(fanIndex),
		}
	}

	// Определяем статус на основе Redfish Status
	status := "OK"
	if fan.Status.State != "Enabled" || fan.Status.Health != "OK" {
		if fan.Status.State == "" {
			status = "N/A"
		} else if fan.Status.Health == "Critical" || fan.Status.Health == "Warning" {
			status = strings.ToUpper(fan.Status.Health)
		} else {
			status = "UNKNOWN"
		}
	}

	// Нормализуем имя и определяем позицию
	name := fan.Name
	if name == "" {
		name = fan.MemberID
	}
	if name == "" {
		name = fmt.Sprintf("Fan%d", fanIndex)
	}

	position := normalizeRedfishFanPosition(name, fan.PhysicalContext)
	fanType := determineRedfishFanType(name, fan.PhysicalContext)

	return FanInfo{
		Name:         name,
		Position:     position,
		CurrentRPM:   fan.Reading,
		TargetRPM:    0, // Обычно не предоставляется через Redfish
		MinRPM:       fan.LowerThresholdCritical,
		MaxRPM:       fan.UpperThresholdCritical,
		Status:       status,
		SensorNumber: uint8(fanIndex),
		FanType:      fanType,
		RawValue:     float64(fan.Reading),
		Units:        fan.ReadingUnits,
	}
}

func normalizeRedfishFanPosition(fanName, physicalContext string) string {
	name := strings.ToUpper(fanName)
	name = strings.ReplaceAll(name, " ", "")

	// Используем PhysicalContext если доступен
	if physicalContext != "" {
		context := strings.ToUpper(physicalContext)
		if strings.Contains(context, "CPU") {
			// Извлекаем номер из имени
			re := regexp.MustCompile(`(\d+)`)
			matches := re.FindStringSubmatch(name)
			if len(matches) > 0 {
				return fmt.Sprintf("CPU%s", matches[0])
			}
			return "CPU1"
		}
		if strings.Contains(context, "CHASSIS") || strings.Contains(context, "SYSTEM") {
			re := regexp.MustCompile(`(\d+)`)
			matches := re.FindStringSubmatch(name)
			if len(matches) > 0 {
				return fmt.Sprintf("CHS%s", matches[0])
			}
			return "CHS1"
		}
	}

	// Обработка различных паттернов имен Redfish вентиляторов
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

func determineRedfishFanType(fanName, physicalContext string) string {
	name := strings.ToUpper(fanName)

	// Используем PhysicalContext если доступен
	if physicalContext != "" {
		context := strings.ToUpper(physicalContext)
		if strings.Contains(context, "CPU") {
			return "CPU"
		}
		if strings.Contains(context, "CHASSIS") || strings.Contains(context, "SYSTEM") {
			return "Chassis"
		}
		if strings.Contains(context, "POWER") || strings.Contains(context, "PSU") {
			return "PSU"
		}
	}

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

// === REDFISH TESTING FUNCTION ===

func testRedfish() error {
	printInfo("Testing Redfish connection...")

	bmcIP, err := getBMCIPAddress()
	if err != nil {
		return fmt.Errorf("failed to get BMC IP: %v", err)
	}

	printSuccess(fmt.Sprintf("BMC IP: %s", bmcIP))

	// Тестируем подключение к Redfish API
	printInfo("Testing Redfish API access...")
	rootData, err := makeRedfishRequest(bmcIP, "/redfish/v1/")
	if err != nil {
		return fmt.Errorf("failed to access Redfish API: %v", err)
	}

	// Парсим root response для получения базовой информации
	var rootService map[string]interface{}
	if err := json.Unmarshal(rootData, &rootService); err != nil {
		printWarning("Failed to parse root service response")
	} else {
		printSuccess("Redfish API access OK")
		if redfishVersion, ok := rootService["RedfishVersion"]; ok {
			printInfo(fmt.Sprintf("  Redfish Version: %v", redfishVersion))
		}
		if name, ok := rootService["Name"]; ok {
			printInfo(fmt.Sprintf("  Service Name: %v", name))
		}
	}

	// Тестируем доступ к thermal информации с retry
	printInfo("Testing thermal sensor access...")
	fans, err := getFanInfoWithRetry(DEFAULT_REDFISH_RETRIES)
	if err != nil {
		printError(fmt.Sprintf("Thermal access failed: %v", err))
		return fmt.Errorf("thermal access required for fan detection")
	} else {
		printSuccess(fmt.Sprintf("Thermal access OK: found %d fan(s)", len(fans)))

		// Показываем статистику по типам вентиляторов
		fanTypes := make(map[string]int)
		for _, fan := range fans {
			fanTypes[fan.FanType]++
		}
		for fanType, count := range fanTypes {
			printInfo(fmt.Sprintf("  %s fans: %d", fanType, count))
		}
	}

	return nil
}

// === HELPER FUNCTIONS ===

func normalizeStatus(status string) string {
	return strings.ToUpper(strings.TrimSpace(status))
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

// === SMART SORTING FUNCTIONS ===

func smartPositionSort(pos1, pos2 string) bool {
	// Разумная сортировка позиций вентиляторов
	// Приоритет: сначала по второму номеру (FAN1_1, FAN2_1, FAN3_1, потом FAN1_2, FAN2_2, FAN3_2)

	// Извлекаем префикс и номера из каждой позиции
	prefix1, num1, subNum1 := extractPrefixAndNumbers(pos1)
	prefix2, num2, subNum2 := extractPrefixAndNumbers(pos2)

	// Сначала сортируем по префиксу
	if prefix1 != prefix2 {
		return prefix1 < prefix2
	}

	// Затем сортируем по второму номеру (subNum)
	if subNum1 != subNum2 {
		return subNum1 < subNum2
	}

	// Если вторые номера одинаковые, сортируем по первому номеру
	return num1 < num2
}

func extractPrefixAndNumbers(position string) (string, int, int) {
	// Ищем паттерн: PREFIX + НОМЕР1 + _ + НОМЕР2
	re := regexp.MustCompile(`^(.+?)(\d+)(?:_(\d+))?$`)
	matches := re.FindStringSubmatch(position)

	if len(matches) >= 3 {
		prefix := matches[1]
		num1 := 0
		num2 := 0

		if num, err := strconv.Atoi(matches[2]); err == nil {
			num1 = num
		}

		// Если есть второй номер после подчеркивания
		if len(matches) >= 4 && matches[3] != "" {
			if num, err := strconv.Atoi(matches[3]); err == nil {
				num2 = num
			}
		}

		return prefix, num1, num2
	}

	// Если не удалось извлечь номера, возвращаем всю строку как префикс
	return position, 0, 0
}

// === HELPER FUNCTIONS FOR VISUALIZATION ===

func parseSlotRange(rangeStr string) (int, int, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format: %s (expected format: 'start-end')", rangeStr)
	}

	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start number in range %s: %v", rangeStr, err)
	}

	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end number in range %s: %v", rangeStr, err)
	}

	if start > end {
		return 0, 0, fmt.Errorf("invalid range %s: start (%d) cannot be greater than end (%d)", rangeStr, start, end)
	}

	return start, end, nil
}

func generateDefaultCustomRows(totalSlots int) CustomRowsConfig {
	var rows []RowConfig

	if totalSlots <= 8 {
		// Single row for small configurations
		rows = append(rows, RowConfig{
			Name:  "Fan Bank 1",
			Slots: fmt.Sprintf("1-%d", totalSlots),
		})
	} else if totalSlots <= 16 {
		// Two rows of 8 each
		rows = append(rows, RowConfig{
			Name:  "Fan Bank 1",
			Slots: "1-8",
		})
		rows = append(rows, RowConfig{
			Name:  "Fan Bank 2",
			Slots: fmt.Sprintf("9-%d", totalSlots),
		})
	} else if totalSlots <= 32 {
		// Multiple rows, try to keep them reasonably sized
		mid := totalSlots / 2
		rows = append(rows, RowConfig{
			Name:  "Fan Bank 1",
			Slots: fmt.Sprintf("1-%d", mid),
		})
		rows = append(rows, RowConfig{
			Name:  "Fan Bank 2",
			Slots: fmt.Sprintf("%d-%d", mid+1, totalSlots),
		})
	} else {
		// Large configurations: break into multiple rows
		slotsPerRow := 16 // Reasonable width for large configurations
		for i := 0; i < totalSlots; i += slotsPerRow {
			end := i + slotsPerRow
			if end > totalSlots {
				end = totalSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Fan Bank %d", len(rows)+1),
				Slots: fmt.Sprintf("%d-%d", i+1, end),
			})
		}
	}

	return CustomRowsConfig{
		Enabled: false, // Disabled by default for backward compatibility
		Rows:    rows,
	}
}

// === CONFIGURATION FUNCTIONS ===

func createDefaultConfig(configPath string, auth *RedfishAuth) error {
	printInfo("Scanning Redfish API for fans to create configuration...")

	allFans, err := getFanInfoWithRetry(DEFAULT_REDFISH_RETRIES)
	if err != nil {
		return fmt.Errorf("could not scan fans via Redfish: %v", err)
	}

	if len(allFans) == 0 {
		printError("No fans found via Redfish API")
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

	printInfo(fmt.Sprintf("Found %d total fan positions via Redfish API:", len(allFans)))
	printInfo(fmt.Sprintf("  - %d active fans", len(activeFans)))
	printInfo(fmt.Sprintf("  - %d physically absent (N/A) fans", len(naFans)))

	// Анализируем паттерны вентиляторов для предложений по игнорированию
	rowAnalysis := analyzeFanRows(allFans)

	var requirements []FanRequirement
	typeVisuals := make(map[string]FanVisual)
	positionToSlot := make(map[string]int)

	// Создаем мэппинг позиций на слоты с сортировкой
	printInfo("Sorting and mapping fan positions to logical slots...")

	// 1) Определяем порядок типов вентиляторов для сортировки
	fanTypeOrder := []string{"CPU", "Chassis", "PSU", "PCIe", "Other"}

	// 2) Группируем вентиляторы по типу
	fansByType := make(map[string][]FanInfo)
	for _, fan := range allFans {
		fansByType[fan.FanType] = append(fansByType[fan.FanType], fan)
	}

	// 3) Для каждого типа сортируем по позиции с учетом номеров
	for _, fanType := range fanTypeOrder {
		if fans, exists := fansByType[fanType]; exists {
			sort.Slice(fans, func(i, j int) bool {
				return smartPositionSort(fans[i].Position, fans[j].Position)
			})
			fansByType[fanType] = fans
		}
	}

	// 4) Присваиваем логические слоты подряд, по группам типов
	slotNum := 1
	for _, fanType := range fanTypeOrder {
		if fans, exists := fansByType[fanType]; exists {
			for _, fan := range fans {
				positionToSlot[fan.Position] = slotNum
				printInfo(fmt.Sprintf("  Mapping %s -> Slot %d (Type: %s, Sensor: %d, Status: %s)",
					fan.Position, slotNum, fan.FanType, fan.SensorNumber, fan.Status))
				slotNum++
			}
		}
	}

	// Группируем вентиляторы по типу (используем уже созданную структуру)
	typeGroups := fansByType

	// Создаем требования и визуалы по типам
	createdVisuals := make(map[string]bool)

	for fanType, fansOfType := range typeGroups {
		if len(fansOfType) == 0 {
			continue // Пропускаем типы без вентиляторов
		}

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

	// Настраиваем аутентификацию
	redfishAuth := RedfishAuth{}
	if auth != nil {
		redfishAuth = *auth
	}

	config := Config{
		FanRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:    typeVisuals,
			PositionToSlot: positionToSlot,
			TotalSlots:     len(allFans),
			SlotWidth:      9,
			IgnoreRows:     ignoreRows,
			MaxRowSlots:    8,
			SlotsPerRow:    8, // Legacy fallback
			CustomRows:     generateDefaultCustomRows(len(allFans)),
		},
		CheckTargetRPM: false,
		RedfishTimeout: 30,
		RedfishAuth:    redfishAuth,
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

	printSuccess("Configuration created successfully using Redfish API data")
	printInfo(fmt.Sprintf("Total fan positions mapped: %d", len(positionToSlot)))
	printInfo(fmt.Sprintf("Visual patterns created for %d fan types", len(typeVisuals)))
	printInfo("All data sourced from Redfish API")
	printInfo("Custom row layout generated (disabled by default)")
	printInfo("To enable custom rows: set 'visualization.custom_rows.enabled' to true")

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
	if config.Visualization.SlotsPerRow == 0 {
		config.Visualization.SlotsPerRow = 8
	}
	if config.Visualization.SlotWidth < 6 {
		config.Visualization.SlotWidth = 9
	}
	if config.RedfishTimeout == 0 {
		config.RedfishTimeout = 30
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
		result.Status = "missing"
		result.Issues = append(result.Issues, "No matching requirements found")
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range matchingReqs {
		if fan.Status == "N/A" {
			if expectedStatus, exists := req.ExpectedStatus[fan.Position]; exists {
				if normalizeStatus(expectedStatus) != "N/A" {
					result.Issues = append(result.Issues, "Expected active fan, got N/A")
					result.StatusOK = false
					hasErrors = true
				}
			}
			continue
		}

		// Проверяем ожидаемый статус для данной позиции
		expectedStatus := ""
		if status, exists := req.ExpectedStatus[fan.Position]; exists {
			expectedStatus = normalizeStatus(status)
		}

		// Если статус не OK, но по конфигурации так и ожидается, пропускаем проверки RPM
		normalizedCurrent := normalizeStatus(fan.Status)
		if normalizedCurrent != "OK" && expectedStatus != "" && expectedStatus != "OK" {
			printDebug(fmt.Sprintf("Skipping RPM checks for %s: expected %s, got %s", fan.Position, expectedStatus, normalizedCurrent))
			continue
		}

		if req.MinRPM > 0 {
			if fan.CurrentRPM == 0 {
				result.Issues = append(result.Issues, "Fan not spinning")
				result.RPMOK = false
				hasErrors = true
			} else if fan.CurrentRPM < req.MinRPM {
				result.Issues = append(result.Issues, fmt.Sprintf("RPM too low: %d (min %d)", fan.CurrentRPM, req.MinRPM))
				result.RPMOK = false
				hasErrors = true
			}
		}

		if req.MaxRPM > 0 && fan.CurrentRPM > req.MaxRPM {
			result.Issues = append(result.Issues, fmt.Sprintf("RPM too high: %d (max %d)", fan.CurrentRPM, req.MaxRPM))
			result.RPMWarn = true
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

	if hasErrors {
		result.Status = "error"
	} else if hasWarnings {
		result.Status = "warning"
	}

	return result
}

func checkFans(config *Config) error {
	printInfo("Starting Redfish fan check...")

	fans, err := getFanInfoWithRetry(DEFAULT_REDFISH_RETRIES)
	if err != nil {
		return fmt.Errorf("failed to get fan info via Redfish: %v", err)
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

	printInfo(fmt.Sprintf("Found total fan positions via Redfish: %d", len(fans)))
	printInfo(fmt.Sprintf("  - Active fans: %d", activeFans))
	printInfo(fmt.Sprintf("  - N/A fans: %d", naFans))

	if len(fans) == 0 {
		printError("No fan positions found via Redfish")
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

			// Пропускаем проверку RPM если статус не OK, но так ожидается
			if normalizedCurrent != "OK" && normalizedExpected != "" && normalizedExpected != "OK" {
				printDebug(fmt.Sprintf("      Skipping RPM checks for %s: expected %s, got %s", fan.Position, normalizedExpected, normalizedCurrent))
			} else {
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
			}

			if normalizedCurrent == "CRITICAL" || normalizedCurrent == "WARNING" {
				if normalizedCurrent == "CRITICAL" {
					printError(fmt.Sprintf("      Status FAILED: %s", normalizedCurrent))
					reqPassed = false
				} else {
					printWarning(fmt.Sprintf("      Status WARNING: %s", normalizedCurrent))
				}
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
		printSuccess(fmt.Sprintf("Redfish validation successful: %d active fans operating correctly, %d N/A fans as expected", activeFans, naFans))
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
		printInfo("Fan Layout (Multi-Row) - Redfish Data:")
	} else {
		printInfo("Fan Layout - Redfish Data:")
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
		maxSlots = len(displayFans)
		if maxSlots == 0 {
			maxSlots = config.Visualization.TotalSlots
		}
	}

	// Создаем массивы слотов
	slotData := make([]FanInfo, maxSlots+1)
	slotResults := make([]FanCheckResult, maxSlots+1)
	foundPositions := make(map[string]bool)
	posToSlot := make(map[int]string)

	// Заполняем слоты отображаемыми вентиляторами
	for _, fan := range displayFans {
		foundPositions[fan.Position] = true
		if logicalSlot, exists := filteredPositionToSlot[fan.Position]; exists {
			if logicalSlot > 0 && logicalSlot <= maxSlots {
				slotData[logicalSlot] = fan
				slotResults[logicalSlot] = checkFanAgainstRequirements(fan, config.FanRequirements)
				posToSlot[logicalSlot] = fan.Position
			}
		}
	}

	// Отмечаем отсутствующие вентиляторы
	required := make(map[string]bool)
	for _, req := range config.FanRequirements {
		for pos, expectedStatus := range req.ExpectedStatus {
			if expectedStatus != "N/A" && expectedStatus != "" {
				required[pos] = true
			}
		}
	}

	for position, expectedSlot := range filteredPositionToSlot {
		if expectedSlot > 0 && expectedSlot <= maxSlots && !foundPositions[position] {
			expectedStatus := getExpectedStatus(position, config.FanRequirements)
			if expectedStatus != "N/A" && expectedStatus != "" {
				slotResults[expectedSlot] = FanCheckResult{Status: "missing"}
				slotData[expectedSlot] = FanInfo{Name: fmt.Sprintf("MISSING:%s", position), Position: position}
				posToSlot[expectedSlot] = position
			}
		}
	}

	// Легенда
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Present & OK  ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Issues       ", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %sMISS%s Missing Req", ColorRed, ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░", ColorReset)
	fmt.Println()

	// Собираем ряды (custom или legacy)
	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		if multiRow {
			// Используем MaxRowSlots для multirow режима
			perRow := config.Visualization.MaxRowSlots
			if perRow == 0 {
				perRow = 8
			}
			for start := 1; start <= maxSlots; start += perRow {
				end := start + perRow - 1
				if end > maxSlots {
					end = maxSlots
				}
				rows = append(rows, RowConfig{
					Name:  fmt.Sprintf("Fan Bank %d", len(rows)+1),
					Slots: fmt.Sprintf("%d-%d", start, end),
				})
			}
		} else {
			// Одна строка для обычного режима
			rows = append(rows, RowConfig{
				Name:  "Fan Layout",
				Slots: fmt.Sprintf("1-%d", maxSlots),
			})
		}
	}

	width := config.Visualization.SlotWidth
	if width < 6 {
		width = 6
	}

	// Визуализация каждого ряда
	for _, row := range rows {
		start, end, err := parseSlotRange(row.Slots)
		if err != nil || start < 1 || end > maxSlots {
			printWarning(fmt.Sprintf("Skipping invalid row '%s': %v", row.Slots, err))
			continue
		}
		count := end - start + 1

		// Заголовок ряда
		if len(rows) > 1 {
			fmt.Printf("%s (Slots %d-%d):\n", row.Name, start, end)
		}

		// Верхняя граница
		fmt.Print("┌")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Строка символов
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			fan := slotData[idx]
			result := slotResults[idx]

			if fan.Name != "" && !strings.HasPrefix(fan.Name, "MISSING:") {
				visual := getFanVisualByType(fan, &config.Visualization)
				sym := centerText(visual.Symbol, width)

				expectedStatus := getExpectedStatus(fan.Position, config.FanRequirements)
				if expectedStatus == "N/A" && fan.Status == "N/A" {
					fmt.Print(ColorGreen + sym + ColorReset)
				} else {
					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + sym + ColorReset)
					case "warning":
						fmt.Print(ColorYellow + sym + ColorReset)
					case "error":
						fmt.Print(ColorRed + sym + ColorReset)
					default:
						fmt.Print(sym)
					}
				}
			} else {
				slotName := posToSlot[idx]
				if required[slotName] || strings.HasPrefix(fan.Name, "MISSING:") {
					miss := centerText("MISS", width)
					fmt.Print(ColorRed + miss + ColorReset)
				} else {
					fmt.Print(centerText("░░░", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Строка типов
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			fan := slotData[idx]
			result := slotResults[idx]

			if fan.Name != "" && !strings.HasPrefix(fan.Name, "MISSING:") {
				visual := getFanVisualByType(fan, &config.Visualization)
				txt := centerText(visual.ShortName, width)

				expectedStatus := getExpectedStatus(fan.Position, config.FanRequirements)
				if expectedStatus == "N/A" && fan.Status == "N/A" {
					fmt.Print(ColorGreen + txt + ColorReset)
				} else {
					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + txt + ColorReset)
					case "warning":
						fmt.Print(ColorYellow + txt + ColorReset)
					case "error":
						fmt.Print(ColorRed + txt + ColorReset)
					default:
						fmt.Print(txt)
					}
				}
			} else {
				slotName := posToSlot[idx]
				if required[slotName] || strings.HasPrefix(fan.Name, "MISSING:") {
					txt := centerText("MISS", width)
					fmt.Print(ColorRed + txt + ColorReset)
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Разделитель
		fmt.Print("├")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┼")
			}
		}
		fmt.Println("┤")

		// Строка RPM
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			fan := slotData[idx]
			result := slotResults[idx]

			if fan.Name != "" && !strings.HasPrefix(fan.Name, "MISSING:") {
				expectedStatus := getExpectedStatus(fan.Position, config.FanRequirements)

				var rpmInfo string
				if expectedStatus == "N/A" && fan.Status == "N/A" {
					rpmInfo = "N/A"
				} else {
					rpmInfo = formatRPM(fan.CurrentRPM)
				}

				rpmText := centerText(rpmInfo, width)

				if expectedStatus == "N/A" && fan.Status == "N/A" {
					fmt.Print(ColorGreen + rpmText + ColorReset)
				} else {
					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + rpmText + ColorReset)
					case "warning":
						fmt.Print(ColorYellow + rpmText + ColorReset)
					case "missing", "error":
						fmt.Print(ColorRed + rpmText + ColorReset)
					default:
						fmt.Print(rpmText)
					}
				}
			} else {
				slotName := posToSlot[idx]
				if required[slotName] || strings.HasPrefix(fan.Name, "MISSING:") {
					txt := centerText("?", width)
					fmt.Print(ColorRed + txt + ColorReset)
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Нижняя граница
		fmt.Print("└")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Подписи - номера слотов
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			slotIdx := start + i
			fmt.Print(centerText(fmt.Sprintf("%d", slotIdx), width+1))
		}
		fmt.Println("(Slot)")

		// Подписи - позиции
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			idx := start + i
			position := posToSlot[idx]
			if position == "" {
				position = "-"
			}
			fmt.Print(centerText(position, width+1))
		}
		fmt.Println("(Position)")

		fmt.Println()
	}

	// Показываем сводку проблем
	errorCount := 0
	warningCount := 0
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		switch result.Status {
		case "error", "missing":
			errorCount++
		case "warning":
			warningCount++
		}
	}

	if errorCount > 0 {
		printError(fmt.Sprintf("Visualization shows %d critical issue(s)", errorCount))
		return fmt.Errorf("critical issues found")
	} else if warningCount > 0 {
		printWarning(fmt.Sprintf("Visualization shows %d warning(s)", warningCount))
	} else {
		printSuccess("All fans visualized successfully")
	}

	return nil
}

// === MAIN FUNCTION ===

func main() {
	var (
		showVersion     = flag.Bool("V", false, "Show version")
		configPath      = flag.String("c", "fan_config.json", "Path to configuration file")
		createConfig    = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag    = flag.Bool("h", false, "Show help")
		listOnly        = flag.Bool("l", false, "List detected fans without configuration check")
		visualize       = flag.Bool("vis", false, "Show visual fan layout")
		multiRow        = flag.Bool("multirow", false, "Show visual layout in multiple rows")
		testRedfishFlag = flag.Bool("test", false, "Test Redfish connection and show basic info")
		debugFlag       = flag.Bool("d", false, "Show detailed debug information")
		authFlag        = flag.String("u", "", "Redfish authentication in format login:password")
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

	// Парсим аутентификацию если предоставлена
	var auth *RedfishAuth
	if *authFlag != "" {
		parts := strings.SplitN(*authFlag, ":", 2)
		if len(parts) == 2 {
			auth = &RedfishAuth{
				Username: parts[0],
				Password: parts[1],
			}
			currentAuth = *auth
			printInfo(fmt.Sprintf("Using authentication: %s", parts[0]))
		} else {
			printError("Invalid authentication format. Use: login:password")
			os.Exit(1)
		}
	}

	// Загружаем конфигурацию для получения сохраненных учетных данных
	if !*createConfig {
		if config, err := loadConfig(*configPath); err == nil {
			if currentAuth.Username == "" && config.RedfishAuth.Username != "" {
				currentAuth = config.RedfishAuth
				printInfo(fmt.Sprintf("Using saved authentication: %s", currentAuth.Username))
			}
		}
	}

	if *testRedfishFlag {
		err := testRedfish()
		if err != nil {
			printError(fmt.Sprintf("Redfish test failed: %v", err))
			os.Exit(1)
		}
		printSuccess("Redfish test completed successfully")
		return
	}

	if *listOnly {
		printInfo("Scanning for fans via Redfish API...")
		fans, err := getFanInfoWithRetry(DEFAULT_REDFISH_RETRIES)
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information via Redfish: %v", err))
			printInfo("Try running with -test flag to diagnose Redfish issues")
			os.Exit(1)
		}

		if len(fans) == 0 {
			printWarning("No fans found via Redfish API")
		} else {
			printSuccess(fmt.Sprintf("Found fans via Redfish API: %d", len(fans)))
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
		err := createDefaultConfig(*configPath, auth)
		if err != nil {
			printError(fmt.Sprintf("Error creating configuration: %v", err))
			printInfo("Try running with -test flag to diagnose Redfish issues")
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully using Redfish API data")
		return
	}

	if *visualize || *multiRow {
		printInfo("Scanning for fans via Redfish API...")
		fans, err := getFanInfoWithRetry(DEFAULT_REDFISH_RETRIES)
		if err != nil {
			printError(fmt.Sprintf("Error getting fan information via Redfish: %v", err))
			printInfo("Try running with -test flag to diagnose Redfish issues")
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		// Обновляем текущую аутентификацию из конфига если не задана
		if currentAuth.Username == "" && config.RedfishAuth.Username != "" {
			currentAuth = config.RedfishAuth
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
		printInfo("Use -test to diagnose Redfish connectivity issues")
		os.Exit(1)
	}

	// Обновляем текущую аутентификацию из конфига если не задана
	if currentAuth.Username == "" && config.RedfishAuth.Username != "" {
		currentAuth = config.RedfishAuth
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkFans(config)
	if err != nil {
		printError(fmt.Sprintf("Fan check failed: %v", err))
		printInfo("Try running with -test flag to diagnose Redfish issues")
		os.Exit(1)
	}
}
