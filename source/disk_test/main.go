package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const VERSION = "1.1.0"

type DiskInfo struct {
	Device       string   `json:"device"`        // Current device path (e.g., /dev/sda)
	CleanID      string   `json:"clean_id"`      // Cleaned identifier without PCI parts
	Model        string   `json:"model"`         // Disk model
	Serial       string   `json:"serial"`        // Serial number
	SizeGB       int      `json:"size_gb"`       // Size in GB
	Type         string   `json:"type"`          // HDD, SSD, NVMe
	Interface    string   `json:"interface"`     // SATA, NVMe, USB, etc.
	Vendor       string   `json:"vendor"`        // Manufacturer
	Firmware     string   `json:"firmware"`      // Firmware version
	RotSpeed     int      `json:"rotation_rpm"`  // Rotation speed for HDDs (0 for SSDs)
	Temperature  int      `json:"temperature"`   // Temperature in Celsius
	SmartStatus  string   `json:"smart_status"`  // SMART overall health (PASSED/FAILED/N/A)
	PowerOnHrs   int      `json:"power_on_hrs"`  // Power-on hours
	PowerCycles  int      `json:"power_cycles"`  // Power cycle count
	PhysicalSlot string   `json:"physical_slot"` // Physical slot identifier from by-path
	LogicalSlot  int      `json:"logical_slot"`  // Logical slot number for visualization
	IsRemovable  bool     `json:"is_removable"`  // USB/external drives
	MountPoints  []string `json:"mount_points"`  // Current mount points
	ByIdPath     string   `json:"by_id_path"`    // Path in /dev/disk/by-id/
	ByPathPath   string   `json:"by_path_path"`  // Path in /dev/disk/by-path/
}

type SlotRequirement struct {
	Name          string `json:"name"`
	MinOccupied   int    `json:"min_occupied"`   // Minimum slots that must have disks
	MaxOccupied   int    `json:"max_occupied"`   // Maximum slots that can have disks
	RequiredType  string `json:"required_type"`  // HDD, SSD, NVMe, any
	MinSizeGB     int    `json:"min_size_gb"`    // Minimum size for disks in these slots
	MaxSizeGB     int    `json:"max_size_gb"`    // Maximum size for disks in these slots
	RequireHealth bool   `json:"require_health"` // Require SMART health OK
	MaxTempC      int    `json:"max_temp_c"`     // Maximum temperature
	RequiredSlots []int  `json:"required_slots"` // Specific slot numbers that must be occupied
	OptionalSlots []int  `json:"optional_slots"` // Slot numbers that may be occupied
	AllowEmpty    bool   `json:"allow_empty"`    // Allow empty slots in required slots
}

type DiskVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type VisualizationConfig struct {
	TypeVisuals map[string]DiskVisual `json:"type_visuals"`     // disk type -> visual
	TotalSlots  int                   `json:"total_slots"`      // Total slots to display
	SlotWidth   int                   `json:"slot_width"`       // Width of each slot
	SlotsPerRow int                   `json:"slots_per_row"`    // Slots per row for multi-row layout
	ShowTemp    bool                  `json:"show_temperature"` // Show temperature row
	ShowSmart   bool                  `json:"show_smart"`       // Show SMART status row
}

type Config struct {
	SlotRequirements []SlotRequirement   `json:"slot_requirements"`
	Visualization    VisualizationConfig `json:"visualization"`
	CheckSmart       bool                `json:"check_smart"`
	CheckTemp        bool                `json:"check_temperature"`
	SmartTimeout     int                 `json:"smart_timeout_seconds"`
}

type SlotCheckResult struct {
	Status     string // "ok", "warning", "error", "empty"
	Issues     []string
	HasDisk    bool
	SizeOK     bool
	TypeOK     bool
	HealthOK   bool
	TempOK     bool
	SizeWarn   bool
	TypeWarn   bool
	HealthWarn bool
	TempWarn   bool
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
	fmt.Printf("Disk Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected disks without configuration check")
	fmt.Println("  -vis        Show visual disk slots layout")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

// ===== ОБНАРУЖЕНИЕ ДИСКОВ ПО ФИЗИЧЕСКИМ СЛОТАМ =====

func getDiskInfo() ([]DiskInfo, error) {
	printDebug("Starting physical slot-based disk detection...")

	// Получаем диски через lsblk
	disks, err := getDisksFromLsblk()
	if err != nil {
		printDebug(fmt.Sprintf("lsblk failed: %v, trying fallback methods", err))
		disks, err = getDisksFromByPath()
		if err != nil {
			return getDisksFromProcPartitions()
		}
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disks found")
	}

	// Обогащаем информацию и определяем физические слоты
	for i := range disks {
		disks[i] = enrichDiskInfo(disks[i])
		disks[i] = determinePhysicalSlot(disks[i])
		disks[i].CleanID = cleanIdentifier(disks[i].ByIdPath)
	}

	// Сортируем по физическим слотам и назначаем логические слоты
	disks = assignLogicalSlots(disks)

	printInfo(fmt.Sprintf("Detected %d disks in physical slots", len(disks)))
	return disks, nil
}

func getDisksFromLsblk() ([]DiskInfo, error) {
	cmd := exec.Command("lsblk", "-J", "-o",
		"NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,WWN,VENDOR,TRAN,ROTA,RM,KNAME,PKNAME")

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk failed: %v", err)
	}

	if debugMode {
		printDebug("lsblk JSON output:")
		printDebug(string(output))
		printDebug("--- End of lsblk output ---")
	}

	return parseLsblkJSON(string(output))
}

type LsblkOutput struct {
	Blockdevices []LsblkDevice `json:"blockdevices"`
}

type LsblkDevice struct {
	Name       string        `json:"name"`
	Size       string        `json:"size"`
	Type       string        `json:"type"`
	Mountpoint *string       `json:"mountpoint"`
	Model      *string       `json:"model"`
	Serial     *string       `json:"serial"`
	WWN        *string       `json:"wwn"`
	Vendor     *string       `json:"vendor"`
	Tran       *string       `json:"tran"`
	Rota       *string       `json:"rota"`
	RM         *string       `json:"rm"`
	Kname      string        `json:"kname"`
	Pkname     *string       `json:"pkname"`
	Children   []LsblkDevice `json:"children,omitempty"`
}

func parseLsblkJSON(jsonStr string) ([]DiskInfo, error) {
	var lsblkOut LsblkOutput
	if err := json.Unmarshal([]byte(jsonStr), &lsblkOut); err != nil {
		return nil, fmt.Errorf("failed to parse lsblk JSON: %v", err)
	}

	var disks []DiskInfo
	processedDevices := make(map[string]bool)

	for _, device := range lsblkOut.Blockdevices {
		if device.Type == "disk" && !processedDevices[device.Kname] {
			disk := parseLsblkDevice(device)
			if disk.Device != "" {
				disks = append(disks, disk)
				processedDevices[device.Kname] = true
			}
		}
	}

	return disks, nil
}

func parseLsblkDevice(device LsblkDevice) DiskInfo {
	disk := DiskInfo{
		Device: "/dev/" + device.Kname,
	}

	if device.Model != nil {
		disk.Model = strings.TrimSpace(*device.Model)
	}
	if device.Serial != nil {
		disk.Serial = strings.TrimSpace(*device.Serial)
	}
	if device.Vendor != nil {
		disk.Vendor = strings.TrimSpace(*device.Vendor)
	}

	disk.SizeGB = parseSizeFromLsblk(device.Size)

	if device.Rota != nil && *device.Rota == "0" {
		disk.Type = "SSD"
	} else if device.Rota != nil && *device.Rota == "1" {
		disk.Type = "HDD"
	}

	if device.Tran != nil {
		switch strings.ToLower(*device.Tran) {
		case "nvme":
			disk.Interface = "NVMe"
			disk.Type = "NVMe"
		case "sata":
			disk.Interface = "SATA"
		case "usb":
			disk.Interface = "USB"
			disk.Type = "USB"
		case "scsi":
			disk.Interface = "SCSI"
		default:
			disk.Interface = strings.ToUpper(*device.Tran)
		}
	}

	if device.RM != nil && *device.RM == "1" {
		disk.IsRemovable = true
		if disk.Type == "" {
			disk.Type = "USB"
		}
	}

	if disk.Type == "" {
		disk.Type = "Unknown"
	}

	if device.Mountpoint != nil && *device.Mountpoint != "" {
		disk.MountPoints = append(disk.MountPoints, *device.Mountpoint)
	}

	for _, child := range device.Children {
		if child.Mountpoint != nil && *child.Mountpoint != "" {
			disk.MountPoints = append(disk.MountPoints, *child.Mountpoint)
		}
	}

	return disk
}

func parseSizeFromLsblk(sizeStr string) int {
	if sizeStr == "" {
		return 0
	}

	sizeStr = strings.ToUpper(strings.TrimSpace(sizeStr))
	re := regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([KMGTPE]?)B?$`)
	matches := re.FindStringSubmatch(sizeStr)

	if len(matches) < 2 {
		return 0
	}

	size, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}

	unit := ""
	if len(matches) > 2 {
		unit = matches[2]
	}

	switch unit {
	case "K":
		return int(size / 1024 / 1024)
	case "M":
		return int(size / 1024)
	case "G", "":
		return int(size)
	case "T":
		return int(size * 1024)
	case "P":
		return int(size * 1024 * 1024)
	case "E":
		return int(size * 1024 * 1024 * 1024)
	default:
		return int(size)
	}
}

// ===== ОПРЕДЕЛЕНИЕ ФИЗИЧЕСКИХ СЛОТОВ =====

func determinePhysicalSlot(disk DiskInfo) DiskInfo {
	// Ищем by-path для определения физического слота
	disk.ByPathPath = findByPathForDevice(disk.Device)
	disk.ByIdPath = findByIdForDevice(disk.Device)

	if disk.ByPathPath != "" {
		disk.PhysicalSlot = extractPhysicalSlotFromPath(disk.ByPathPath)
	} else {
		// Fallback: генерируем слот из device name
		disk.PhysicalSlot = generateSlotFromDevice(disk.Device)
	}

	if debugMode {
		printDebug(fmt.Sprintf("Device %s -> Physical slot: %s (from path: %s)",
			disk.Device, disk.PhysicalSlot, disk.ByPathPath))
	}

	return disk
}

func findByPathForDevice(device string) string {
	byPathDir := "/dev/disk/by-path"
	entries, err := os.ReadDir(byPathDir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		pathLink := filepath.Join(byPathDir, entry.Name())
		realDevice, err := filepath.EvalSymlinks(pathLink)
		if err != nil {
			continue
		}

		if realDevice == device {
			return pathLink
		}
	}

	return ""
}

func findByIdForDevice(device string) string {
	byIdDir := "/dev/disk/by-id"
	entries, err := os.ReadDir(byIdDir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() || isPartitionByIdName(entry.Name()) {
			continue
		}

		idPath := filepath.Join(byIdDir, entry.Name())
		realDevice, err := filepath.EvalSymlinks(idPath)
		if err != nil {
			continue
		}

		if realDevice == device {
			return idPath
		}
	}

	return ""
}

func extractPhysicalSlotFromPath(byPathPath string) string {
	filename := filepath.Base(byPathPath)

	// Примеры путей:
	// pci-0000:00:17.0-ata-1 -> SATA1
	// pci-0000:03:00.0-nvme-1 -> NVMe1
	// pci-0000:00:14.0-usb-0:1:1.0-scsi-0:0:0:0 -> USB1

	// Ищем SATA слоты
	if strings.Contains(filename, "-ata-") {
		re := regexp.MustCompile(`-ata-(\d+)`)
		if matches := re.FindStringSubmatch(filename); len(matches) > 1 {
			return fmt.Sprintf("SATA%s", matches[1])
		}
	}

	// Ищем NVMe слоты
	if strings.Contains(filename, "-nvme-") {
		re := regexp.MustCompile(`-nvme-(\d+)`)
		if matches := re.FindStringSubmatch(filename); len(matches) > 1 {
			return fmt.Sprintf("NVMe%s", matches[1])
		}
	}

	// Ищем USB порты
	if strings.Contains(filename, "-usb-") {
		re := regexp.MustCompile(`-usb-[^-]+-[^-]+-(\d+):`)
		if matches := re.FindStringSubmatch(filename); len(matches) > 1 {
			return fmt.Sprintf("USB%s", matches[1])
		}
	}

	// Fallback: извлекаем последнюю цифру
	re := regexp.MustCompile(`(\d+)(?!.*\d)`)
	if matches := re.FindStringSubmatch(filename); len(matches) > 0 {
		return fmt.Sprintf("SLOT%s", matches[0])
	}

	return "UNKNOWN"
}

func generateSlotFromDevice(device string) string {
	deviceName := filepath.Base(device)

	if strings.HasPrefix(deviceName, "nvme") {
		re := regexp.MustCompile(`nvme(\d+)`)
		if matches := re.FindStringSubmatch(deviceName); len(matches) > 1 {
			return fmt.Sprintf("NVMe%s", matches[1])
		}
		return "NVMe1"
	}

	if strings.HasPrefix(deviceName, "sd") {
		if len(deviceName) >= 3 {
			letter := deviceName[2]
			slotNum := int(letter-'a') + 1
			return fmt.Sprintf("SATA%d", slotNum)
		}
	}

	if strings.HasPrefix(deviceName, "vd") {
		if len(deviceName) >= 3 {
			letter := deviceName[2]
			slotNum := int(letter-'a') + 1
			return fmt.Sprintf("VirtIO%d", slotNum)
		}
	}

	if strings.HasPrefix(deviceName, "mmcblk") {
		re := regexp.MustCompile(`mmcblk(\d+)`)
		if matches := re.FindStringSubmatch(deviceName); len(matches) > 1 {
			slotNum, _ := strconv.Atoi(matches[1])
			return fmt.Sprintf("MMC%d", slotNum+1)
		}
		return "MMC1"
	}

	return strings.ToUpper(deviceName)
}

// ===== ОЧИСТКА ИДЕНТИФИКАТОРОВ =====

func cleanIdentifier(byIdPath string) string {
	if byIdPath == "" {
		return ""
	}

	filename := filepath.Base(byIdPath)

	// Удаляем PCI-специфичные части из конца
	// Примеры:
	// Netac_OnlyDisk_FC172C264DB74280-0:0 -> Netac_OnlyDisk_FC172C264DB74280
	// SAMSUNG_SSD_980_PRO_1TB_S6B1NX0W123456A-0:0 -> SAMSUNG_SSD_980_PRO_1TB_S6B1NX0W123456A

	// Удаляем PCI координаты в конце
	re := regexp.MustCompile(`-\d+:\d+$`)
	cleaned := re.ReplaceAllString(filename, "")

	// Удаляем префиксы типа ata-, nvme-, scsi-, usb-
	prefixes := []string{"ata-", "nvme-", "scsi-", "usb-", "wwn-"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(cleaned, prefix) {
			cleaned = strings.TrimPrefix(cleaned, prefix)
			break
		}
	}

	return cleaned
}

// ===== НАЗНАЧЕНИЕ ЛОГИЧЕСКИХ СЛОТОВ =====

func assignLogicalSlots(disks []DiskInfo) []DiskInfo {
	// Сортируем диски по физическим слотам для последовательной нумерации
	sort.Slice(disks, func(i, j int) bool {
		return comparePhysicalSlots(disks[i].PhysicalSlot, disks[j].PhysicalSlot)
	})

	// Назначаем логические слоты
	for i := range disks {
		disks[i].LogicalSlot = i + 1
	}

	return disks
}

func comparePhysicalSlots(a, b string) bool {
	// Сортируем слоты в логическом порядке
	// SATA1, SATA2, ..., NVMe1, NVMe2, ..., USB1, USB2, ...

	getSlotType := func(slot string) int {
		if strings.HasPrefix(slot, "SATA") {
			return 1
		} else if strings.HasPrefix(slot, "NVMe") {
			return 2
		} else if strings.HasPrefix(slot, "USB") {
			return 3
		} else if strings.HasPrefix(slot, "MMC") {
			return 4
		} else if strings.HasPrefix(slot, "VirtIO") {
			return 5
		}
		return 99
	}

	getSlotNumber := func(slot string) int {
		re := regexp.MustCompile(`(\d+)`)
		if matches := re.FindString(slot); matches != "" {
			if num, err := strconv.Atoi(matches); err == nil {
				return num
			}
		}
		return 0
	}

	typeA, typeB := getSlotType(a), getSlotType(b)
	if typeA != typeB {
		return typeA < typeB
	}

	numA, numB := getSlotNumber(a), getSlotNumber(b)
	return numA < numB
}

// ===== ПРОВЕРКА СЛОТОВ =====

func checkSlotRequirements(disks []DiskInfo, config *Config) error {
	printInfo("Starting slot-based disk check...")

	maxSlot := config.Visualization.TotalSlots
	if maxSlot == 0 {
		maxSlot = len(disks) + 2
	}

	// Создаём массив слотов
	slots := make([]DiskInfo, maxSlot+1) // +1 because slots start from 1

	// Заполняем слоты дисками
	for _, disk := range disks {
		if disk.LogicalSlot > 0 && disk.LogicalSlot <= maxSlot {
			slots[disk.LogicalSlot] = disk
		}
	}

	// Проверяем каждое требование
	allPassed := true
	for _, req := range config.SlotRequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		if !checkSlotRequirement(slots, req, config) {
			allPassed = false
		}
	}

	if allPassed {
		printSuccess("All slot requirements passed")
	} else {
		printError("Some slot requirements failed")
		return fmt.Errorf("slot requirements not met")
	}

	return nil
}

func checkSlotRequirement(slots []DiskInfo, req SlotRequirement, config *Config) bool {
	occupiedCount := 0
	reqPassed := true

	// Подсчитываем занятые слоты
	for i := 1; i < len(slots); i++ {
		if slots[i].Device != "" {
			occupiedCount++
		}
	}

	printInfo(fmt.Sprintf("  Total occupied slots: %d", occupiedCount))

	// Проверяем минимальное/максимальное количество
	if req.MinOccupied > 0 && occupiedCount < req.MinOccupied {
		printError(fmt.Sprintf("  Not enough occupied slots: %d (required %d)", occupiedCount, req.MinOccupied))
		reqPassed = false
	}

	if req.MaxOccupied > 0 && occupiedCount > req.MaxOccupied {
		printError(fmt.Sprintf("  Too many occupied slots: %d (maximum %d)", occupiedCount, req.MaxOccupied))
		reqPassed = false
	}

	// Проверяем обязательные слоты
	for _, slotNum := range req.RequiredSlots {
		if slotNum > 0 && slotNum < len(slots) {
			disk := slots[slotNum]
			if disk.Device == "" {
				if !req.AllowEmpty {
					printError(fmt.Sprintf("  Required slot %d is empty", slotNum))
					reqPassed = false
				}
			} else {
				printInfo(fmt.Sprintf("    Slot %d: %s (%s, %dGB)", slotNum, disk.CleanID, disk.Type, disk.SizeGB))

				// Проверяем требования к диску в этом слоте
				result := checkDiskInSlot(disk, req, config)
				if result.Status == "error" {
					reqPassed = false
				}

				for _, issue := range result.Issues {
					if result.Status == "error" {
						printError(fmt.Sprintf("      %s", issue))
					} else {
						printWarning(fmt.Sprintf("      %s", issue))
					}
				}
			}
		}
	}

	// Проверяем опциональные слоты
	for _, slotNum := range req.OptionalSlots {
		if slotNum > 0 && slotNum < len(slots) {
			disk := slots[slotNum]
			if disk.Device != "" {
				printInfo(fmt.Sprintf("    Optional slot %d: %s (%s, %dGB)", slotNum, disk.CleanID, disk.Type, disk.SizeGB))

				result := checkDiskInSlot(disk, req, config)
				for _, issue := range result.Issues {
					if result.Status == "error" {
						printError(fmt.Sprintf("      %s", issue))
						reqPassed = false
					} else {
						printWarning(fmt.Sprintf("      %s", issue))
					}
				}
			}
		}
	}

	if reqPassed {
		printSuccess(fmt.Sprintf("  Requirement PASSED: %s", req.Name))
	} else {
		printError(fmt.Sprintf("  Requirement FAILED: %s", req.Name))
	}

	return reqPassed
}

func checkDiskInSlot(disk DiskInfo, req SlotRequirement, config *Config) SlotCheckResult {
	result := SlotCheckResult{
		Status:   "ok",
		HasDisk:  true,
		SizeOK:   true,
		TypeOK:   true,
		HealthOK: true,
		TempOK:   true,
	}

	// Проверка типа
	if req.RequiredType != "" && req.RequiredType != "any" && disk.Type != req.RequiredType {
		result.Issues = append(result.Issues,
			fmt.Sprintf("Type mismatch: %s (required %s)", disk.Type, req.RequiredType))
		result.TypeOK = false
		result.Status = "error"
	}

	// Проверка размера
	if req.MinSizeGB > 0 && disk.SizeGB < req.MinSizeGB {
		result.Issues = append(result.Issues,
			fmt.Sprintf("Size too small: %dGB (required %dGB)", disk.SizeGB, req.MinSizeGB))
		result.SizeOK = false
		result.Status = "error"
	}

	if req.MaxSizeGB > 0 && disk.SizeGB > req.MaxSizeGB {
		result.Issues = append(result.Issues,
			fmt.Sprintf("Size too large: %dGB (max %dGB)", disk.SizeGB, req.MaxSizeGB))
		result.SizeOK = false
		result.Status = "error"
	}

	// Проверка здоровья (если включена)
	if config.CheckSmart && req.RequireHealth {
		if disk.SmartStatus == "N/A" {
			result.Issues = append(result.Issues, "SMART status unavailable")
			result.HealthWarn = true
			if result.Status == "ok" {
				result.Status = "warning"
			}
		} else if disk.SmartStatus == "FAILED" {
			result.Issues = append(result.Issues, "SMART health check failed")
			result.HealthOK = false
			result.Status = "error"
		}
	}

	// Проверка температуры (если включена)
	if config.CheckTemp && req.MaxTempC > 0 && disk.Temperature > 0 {
		if disk.Temperature > req.MaxTempC {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Temperature too high: %d°C (max %d°C)", disk.Temperature, req.MaxTempC))
			result.TempOK = false
			result.Status = "error"
		}
	}

	return result
}

// ===== ВИЗУАЛИЗАЦИЯ =====

func visualizeSlots(disks []DiskInfo, config *Config) error {
	printInfo("Disk Slots Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(disks) + 2
	}

	// Создаём данные слотов
	slotData := make([]DiskInfo, maxSlots+1)
	slotResults := make([]SlotCheckResult, maxSlots+1)

	// Заполняем слоты
	for _, disk := range disks {
		if disk.LogicalSlot > 0 && disk.LogicalSlot <= maxSlots {
			slotData[disk.LogicalSlot] = disk
			slotResults[disk.LogicalSlot] = checkSlotForVisualization(disk, config)
		}
	}

	// Проверяем пустые слоты
	hasErrors := false
	hasWarnings := false

	for i := 1; i <= maxSlots; i++ {
		if slotData[i].Device == "" {
			slotResults[i] = SlotCheckResult{Status: "empty", HasDisk: false}
		}

		status := slotResults[i].Status
		if status == "error" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Легенда
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Disk Present & OK  ", ColorGreen, "████", ColorReset)
	fmt.Printf("  %s%s%s Disk with Issues  ", ColorYellow, "████", ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░░", ColorReset)
	fmt.Println()

	// Рендерим визуализацию
	renderSlotVisualization(slotData, slotResults, config, maxSlots)

	// Финальный статус
	if hasErrors {
		printError("Slot configuration validation FAILED!")
		return fmt.Errorf("slot configuration validation failed")
	} else if hasWarnings {
		printWarning("Slot configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All slots meet requirements!")
		return nil
	}
}

func checkSlotForVisualization(disk DiskInfo, config *Config) SlotCheckResult {
	result := SlotCheckResult{
		Status:   "ok",
		HasDisk:  true,
		SizeOK:   true,
		TypeOK:   true,
		HealthOK: true,
		TempOK:   true,
	}

	// Основные проверки здоровья диска
	if config.CheckSmart && disk.SmartStatus == "FAILED" {
		result.Issues = append(result.Issues, "SMART health failed")
		result.HealthOK = false
		result.Status = "error"
	} else if config.CheckSmart && disk.SmartStatus == "N/A" {
		result.Issues = append(result.Issues, "SMART unavailable")
		result.HealthWarn = true
		if result.Status == "ok" {
			result.Status = "warning"
		}
	}

	if config.CheckTemp && disk.Temperature > 70 {
		result.Issues = append(result.Issues, fmt.Sprintf("High temperature: %d°C", disk.Temperature))
		result.TempOK = false
		result.Status = "error"
	}

	return result
}

func renderSlotVisualization(slotData []DiskInfo, slotResults []SlotCheckResult, config *Config, maxSlots int) {
	width := config.Visualization.SlotWidth
	slotsPerRow := config.Visualization.SlotsPerRow
	if slotsPerRow == 0 {
		slotsPerRow = 6
	}

	for rowStart := 1; rowStart <= maxSlots; rowStart += slotsPerRow {
		rowEnd := rowStart + slotsPerRow - 1
		if rowEnd > maxSlots {
			rowEnd = maxSlots
		}
		rowSlots := rowEnd - rowStart + 1

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
			slotIdx := rowStart + i
			visual := getDiskVisual(slotData[slotIdx], &config.Visualization)
			result := slotResults[slotIdx]

			symbolText := centerText(visual.Symbol, width)

			switch result.Status {
			case "ok":
				fmt.Print(ColorGreen + symbolText + ColorReset)
			case "warning":
				fmt.Print(ColorYellow + symbolText + ColorReset)
			case "error":
				fmt.Print(ColorRed + symbolText + ColorReset)
			case "empty":
				fmt.Print(centerText("░░░░", width))
			default:
				fmt.Print(symbolText)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Ряд типов
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			result := slotResults[slotIdx]

			if slotData[slotIdx].Device != "" {
				visual := getDiskVisual(slotData[slotIdx], &config.Visualization)
				nameText := centerText(visual.ShortName, width)

				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + nameText + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + nameText + ColorReset)
				case "error":
					fmt.Print(ColorRed + nameText + ColorReset)
				default:
					fmt.Print(nameText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Ряд размеров
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			result := slotResults[slotIdx]

			if slotData[slotIdx].Device != "" {
				sizeText := centerText(formatSize(slotData[slotIdx].SizeGB), width)

				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + sizeText + ColorReset)
				case "warning":
					fmt.Print(ColorYellow + sizeText + ColorReset)
				case "error":
					fmt.Print(ColorRed + sizeText + ColorReset)
				default:
					fmt.Print(sizeText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Дополнительные ряды если включены
		if config.Visualization.ShowTemp {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := rowStart + i
				result := slotResults[slotIdx]

				if slotData[slotIdx].Device != "" {
					tempText := centerText(formatTemp(slotData[slotIdx].Temperature), width)

					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + tempText + ColorReset)
					case "warning":
						fmt.Print(ColorYellow + tempText + ColorReset)
					case "error":
						fmt.Print(ColorRed + tempText + ColorReset)
					default:
						fmt.Print(tempText)
					}
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
				fmt.Print("│")
			}
			fmt.Println()
		}

		// Нижняя граница
		fmt.Print("└")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Подписи слотов
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			fmt.Print(centerText(fmt.Sprintf("%d", slotIdx), width+1))
		}
		fmt.Println("(Slot)")

		// Подписи физических слотов
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			physSlot := ""
			if slotData[slotIdx].Device != "" {
				physSlot = slotData[slotIdx].PhysicalSlot
				if len(physSlot) > width {
					physSlot = physSlot[:width-3] + "..."
				}
			} else {
				physSlot = "-"
			}
			fmt.Print(centerText(physSlot, width+1))
		}
		fmt.Println("(Physical)")

		// Подписи устройств
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			device := ""
			if slotData[slotIdx].Device != "" {
				device = filepath.Base(slotData[slotIdx].Device)
			} else {
				device = "-"
			}
			fmt.Print(centerText(device, width+1))
		}
		fmt.Println("(Dev)")

		fmt.Println()
	}
}

// ===== ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ =====

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for disk information to create configuration...")

	disks, err := getDiskInfo()
	if err != nil {
		return fmt.Errorf("could not scan disks: %v", err)
	}

	if len(disks) == 0 {
		return fmt.Errorf("no disks found - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d disk(s) in physical slots:", len(disks)))

	// Группируем диски по типам
	typeGroups := make(map[string][]DiskInfo)
	for _, disk := range disks {
		typeGroups[disk.Type] = append(typeGroups[disk.Type], disk)
	}

	var requirements []SlotRequirement
	typeVisuals := make(map[string]DiskVisual)

	// Показываем найденные диски
	for _, disk := range disks {
		printInfo(fmt.Sprintf("  Slot %d: %s (%s, %dGB, %s)",
			disk.LogicalSlot, disk.CleanID, disk.Type, disk.SizeGB, disk.PhysicalSlot))
	}

	// Проверяем SMART
	globalHasHealth := false
	for _, disk := range disks {
		if disk.SmartStatus == "PASSED" {
			globalHasHealth = true
			break
		}
	}

	// Создаём требования по типам
	for diskType, disksOfType := range typeGroups {
		printInfo(fmt.Sprintf("  Processing %d %s disk(s):", len(disksOfType), diskType))

		var requiredSlots []int
		for _, disk := range disksOfType {
			requiredSlots = append(requiredSlots, disk.LogicalSlot)
		}

		minSize := 0
		for _, disk := range disksOfType {
			if minSize == 0 || (disk.SizeGB > 0 && disk.SizeGB < minSize) {
				minSize = disk.SizeGB
			}
		}

		req := SlotRequirement{
			Name:          fmt.Sprintf("%s slots (%d occupied)", diskType, len(disksOfType)),
			MinOccupied:   len(disksOfType),
			RequiredType:  diskType,
			MinSizeGB:     minSize,
			RequireHealth: globalHasHealth,
			MaxTempC:      70,
			RequiredSlots: requiredSlots,
			AllowEmpty:    false,
		}

		requirements = append(requirements, req)

		// Создаём визуальные элементы
		visual := generateDiskVisual(diskType)
		typeVisuals[diskType] = visual
	}

	config := Config{
		SlotRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals: typeVisuals,
			TotalSlots:  len(disks) + 2,
			SlotWidth:   12,
			SlotsPerRow: 6,
			ShowTemp:    true,
			ShowSmart:   true,
		},
		CheckSmart:   globalHasHealth,
		CheckTemp:    true,
		SmartTimeout: 10,
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

	printSuccess("Configuration created successfully based on physical slots")
	printInfo(fmt.Sprintf("Total slots configured: %d", len(disks)))
	printInfo("Configuration focuses on slot occupancy rather than specific devices")
	if globalHasHealth {
		printInfo("SMART checking enabled")
	}

	return nil
}

// Остальные функции без изменений...
func isPartitionByIdName(name string) bool {
	partitionPatterns := []string{"-part", "_part"}
	for _, pattern := range partitionPatterns {
		if strings.Contains(name, pattern) {
			return true
		}
	}

	if len(name) > 0 {
		lastChar := name[len(name)-1]
		if lastChar >= '1' && lastChar <= '9' {
			if len(name) >= 2 {
				secondLastChar := name[len(name)-2]
				if secondLastChar == '-' || secondLastChar == '_' || secondLastChar == 'p' {
					return true
				}
			}
		}
	}

	return false
}

func getDisksFromByPath() ([]DiskInfo, error) {
	return nil, fmt.Errorf("by-path fallback not implemented")
}

func getDisksFromProcPartitions() ([]DiskInfo, error) {
	return nil, fmt.Errorf("proc partitions fallback not implemented")
}

func enrichDiskInfo(disk DiskInfo) DiskInfo {
	// Получаем размер если не определён
	if disk.SizeGB == 0 {
		disk.SizeGB = getDiskSizeFromDevice(disk.Device)
	}

	// Обогащаем из sysfs
	disk = enrichFromSysBlock(disk)

	// Получаем точки монтирования
	disk.MountPoints = getMountPoints(disk.Device)

	// SMART данные
	if smart, err := getSmartInfo(disk.Device); err == nil {
		disk.SmartStatus = smart.Status
		disk.Temperature = smart.Temperature
		disk.PowerOnHrs = smart.PowerOnHours
		disk.PowerCycles = smart.PowerCycles
		if disk.Firmware == "" {
			disk.Firmware = smart.Firmware
		}
		if disk.RotSpeed == 0 {
			disk.RotSpeed = smart.RotationRate
		}
	} else {
		disk.SmartStatus = "N/A"
		if debugMode {
			printDebug(fmt.Sprintf("SMART data unavailable for %s: %v", disk.Device, err))
		}
	}

	return disk
}

func getDiskSizeFromDevice(device string) int {
	deviceName := filepath.Base(device)
	sizePath := fmt.Sprintf("/sys/block/%s/size", deviceName)

	data, err := os.ReadFile(sizePath)
	if err != nil {
		return 0
	}

	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}

	bytes := sectors * 512
	gb := bytes / 1024 / 1024 / 1024

	return int(gb)
}

func enrichFromSysBlock(disk DiskInfo) DiskInfo {
	deviceName := strings.TrimPrefix(disk.Device, "/dev/")
	sysPath := fmt.Sprintf("/sys/block/%s", deviceName)

	if _, err := os.Stat(sysPath); os.IsNotExist(err) {
		disk.Type = "Unknown"
		return disk
	}

	if model, err := readSysFile(sysPath + "/device/model"); err == nil {
		disk.Model = strings.TrimSpace(model)
	}

	if vendor, err := readSysFile(sysPath + "/device/vendor"); err == nil {
		disk.Vendor = strings.TrimSpace(vendor)
	}

	if serial, err := readSysFile(sysPath + "/device/serial"); err == nil {
		if disk.Serial == "" {
			disk.Serial = strings.TrimSpace(serial)
		}
	}

	if disk.Type == "" || disk.Type == "Unknown" {
		disk.Type = determineDiskTypeFromSys(deviceName, sysPath)
	}

	if disk.Interface == "" {
		disk.Interface = determineInterface(deviceName, sysPath)
	}

	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		disk.IsRemovable = strings.TrimSpace(removable) == "1"
	}

	if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
		if strings.TrimSpace(rotational) == "1" {
			if rate, err := readSysFile(sysPath + "/device/rotation_rate"); err == nil {
				if rpm, parseErr := strconv.Atoi(strings.TrimSpace(rate)); parseErr == nil {
					disk.RotSpeed = rpm
				}
			}
		}
	}

	return disk
}

func readSysFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func determineDiskTypeFromSys(deviceName, sysPath string) string {
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
		if strings.TrimSpace(rotational) == "0" {
			return "SSD"
		} else {
			return "HDD"
		}
	}

	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		if strings.TrimSpace(removable) == "1" {
			return "USB"
		}
	}

	return "Unknown"
}

func determineInterface(deviceName, sysPath string) string {
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	if devicePath, err := os.Readlink(sysPath + "/device"); err == nil {
		if strings.Contains(devicePath, "usb") {
			return "USB"
		}
		if strings.Contains(devicePath, "ata") {
			return "SATA"
		}
		if strings.Contains(devicePath, "scsi") {
			return "SCSI"
		}
	}

	if strings.HasPrefix(deviceName, "sd") {
		return "SATA"
	}

	return "Unknown"
}

func getMountPoints(device string) []string {
	var mountPoints []string

	file, err := os.Open("/proc/mounts")
	if err != nil {
		return mountPoints
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			mountedDevice := fields[0]
			mountPoint := fields[1]

			if strings.HasPrefix(mountedDevice, device) {
				mountPoints = append(mountPoints, mountPoint)
			}
		}
	}

	return mountPoints
}

type SmartData struct {
	Status       string
	Temperature  int
	PowerOnHours int
	PowerCycles  int
	Firmware     string
	RotationRate int
}

func getSmartInfo(device string) (SmartData, error) {
	var smart SmartData

	if _, err := exec.LookPath("smartctl"); err != nil {
		return smart, fmt.Errorf("smartctl not found")
	}

	cmd := exec.Command("smartctl", "-a", device)
	output, err := cmd.Output()
	if err != nil {
		return smart, fmt.Errorf("smartctl failed: %v", err)
	}

	outputStr := string(output)
	if debugMode {
		printDebug(fmt.Sprintf("SMART data for %s:", device))
		printDebug(outputStr)
		printDebug("--- End of SMART data ---")
	}

	smart = parseSmartOutput(outputStr)
	return smart, nil
}

func parseSmartOutput(output string) SmartData {
	var smart SmartData
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.Contains(line, "SMART overall-health") {
			if strings.Contains(line, "PASSED") {
				smart.Status = "PASSED"
			} else if strings.Contains(line, "FAILED") {
				smart.Status = "FAILED"
			}
		}

		if strings.Contains(line, "Temperature_Celsius") || strings.Contains(line, "Current Drive Temperature") {
			if temp := extractNumber(line); temp > 0 && temp < 200 {
				smart.Temperature = temp
			}
		}

		if strings.Contains(line, "Power_On_Hours") {
			smart.PowerOnHours = extractNumber(line)
		}

		if strings.Contains(line, "Power_Cycle_Count") {
			smart.PowerCycles = extractNumber(line)
		}

		if strings.Contains(line, "Firmware Version:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				smart.Firmware = strings.TrimSpace(parts[1])
			}
		}

		if strings.Contains(line, "Rotation Rate:") {
			if strings.Contains(line, "Solid State Device") {
				smart.RotationRate = 0
			} else {
				smart.RotationRate = extractNumber(line)
			}
		}
	}

	return smart
}

func extractNumber(line string) int {
	re := regexp.MustCompile(`\b(\d+)\b`)
	matches := re.FindAllString(line, -1)

	for _, match := range matches {
		if num, err := strconv.Atoi(match); err == nil {
			if num > 0 {
				return num
			}
		}
	}
	return 0
}

func generateDiskVisual(diskType string) DiskVisual {
	visual := DiskVisual{
		Description: fmt.Sprintf("%s Drive", diskType),
		Color:       "green",
	}

	switch diskType {
	case "NVMe":
		visual.Symbol = "▓▓▓▓"
		visual.ShortName = "NVMe"
	case "SSD":
		visual.Symbol = "████"
		visual.ShortName = "SSD"
	case "HDD":
		visual.Symbol = "▓░▓░"
		visual.ShortName = "HDD"
	case "USB":
		visual.Symbol = "░░░░"
		visual.ShortName = "USB"
	default:
		visual.Symbol = "░▓░▓"
		visual.ShortName = "DISK"
	}

	return visual
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.SmartTimeout == 0 {
		cfg.SmartTimeout = 10
	}

	return &cfg, nil
}

func getDiskVisual(disk DiskInfo, config *VisualizationConfig) DiskVisual {
	if disk.Device == "" {
		return DiskVisual{
			Symbol:      "░░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	diskType := disk.Type
	if diskType == "" {
		diskType = "Unknown"
	}

	if visual, exists := config.TypeVisuals[diskType]; exists {
		return visual
	}

	return generateDiskVisual(diskType)
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

func formatSize(sizeGB int) string {
	if sizeGB == 0 {
		return "?"
	}
	if sizeGB < 1024 {
		return fmt.Sprintf("%dGB", sizeGB)
	} else {
		return fmt.Sprintf("%.1fTB", float64(sizeGB)/1024.0)
	}
}

func formatTemp(temp int) string {
	if temp == 0 {
		return "?"
	}
	return fmt.Sprintf("%d°C", temp)
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "disk_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected disks without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual disk slots layout")
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
		printInfo("Scanning for disk drives in physical slots...")
		disks, err := getDiskInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting disk information: %v", err))
			os.Exit(1)
		}

		if len(disks) == 0 {
			printWarning("No disk drives found")
		} else {
			printSuccess(fmt.Sprintf("Found disk drives: %d", len(disks)))
			for _, disk := range disks {
				fmt.Printf("\nLogical Slot %d:\n", disk.LogicalSlot)
				fmt.Printf("  Clean ID: %s\n", disk.CleanID)
				fmt.Printf("  Device: %s\n", disk.Device)
				fmt.Printf("  Physical Slot: %s\n", disk.PhysicalSlot)
				fmt.Printf("  Model: %s\n", disk.Model)
				fmt.Printf("  Serial: %s\n", disk.Serial)
				fmt.Printf("  Size: %s\n", formatSize(disk.SizeGB))
				fmt.Printf("  Type: %s\n", disk.Type)
				fmt.Printf("  Interface: %s\n", disk.Interface)
				fmt.Printf("  Vendor: %s\n", disk.Vendor)
				fmt.Printf("  Firmware: %s\n", disk.Firmware)
				fmt.Printf("  Rotation Speed: %d RPM\n", disk.RotSpeed)
				fmt.Printf("  Temperature: %s\n", formatTemp(disk.Temperature))
				fmt.Printf("  SMART Status: %s\n", disk.SmartStatus)
				fmt.Printf("  Power-On Hours: %d\n", disk.PowerOnHrs)
				fmt.Printf("  Power Cycles: %d\n", disk.PowerCycles)
				fmt.Printf("  Removable: %t\n", disk.IsRemovable)
				if disk.ByIdPath != "" {
					fmt.Printf("  By-ID Path: %s\n", disk.ByIdPath)
				}
				if disk.ByPathPath != "" {
					fmt.Printf("  By-Path: %s\n", disk.ByPathPath)
				}
				if len(disk.MountPoints) > 0 {
					fmt.Printf("  Mount Points: %s\n", strings.Join(disk.MountPoints, ", "))
				}
			}
		}
		return
	}

	if *visualize {
		printInfo("Scanning for disk drives...")
		disks, err := getDiskInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting disk information: %v", err))
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		err = visualizeSlots(disks, config)
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
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully")
		return
	}

	// Load configuration and perform check
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found disks")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	disks, err := getDiskInfo()
	if err != nil {
		printError(fmt.Sprintf("Failed to get disk info: %v", err))
		os.Exit(1)
	}

	err = checkSlotRequirements(disks, config)
	if err != nil {
		printError(fmt.Sprintf("Disk check failed: %v", err))
		os.Exit(1)
	}
}
