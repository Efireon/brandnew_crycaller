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

const VERSION = "1.1.1"

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
			printDebug(fmt.Sprintf("by-path failed: %v, trying proc/partitions", err))
			return getDisksFromProcPartitions()
		}
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disks found")
	}

	printDebug(fmt.Sprintf("Found %d raw disks before enrichment:", len(disks)))
	for i, disk := range disks {
		printDebug(fmt.Sprintf("  Raw disk %d: %s (Type: %s, Size: %dGB)", i+1, disk.Device, disk.Type, disk.SizeGB))
	}

	// Обогащаем информацию и определяем физические слоты
	for i := range disks {
		printDebug(fmt.Sprintf("Enriching disk %s...", disks[i].Device))
		disks[i] = enrichDiskInfo(disks[i])
		disks[i] = determinePhysicalSlot(disks[i])
		disks[i].CleanID = cleanIdentifier(disks[i].ByIdPath)

		printDebug(fmt.Sprintf("  After enrichment: Type=%s, Interface=%s, PhysicalSlot=%s, SMART=%s, Temp=%d°C",
			disks[i].Type, disks[i].Interface, disks[i].PhysicalSlot, disks[i].SmartStatus, disks[i].Temperature))
	}

	// Сортируем по физическим слотам и назначаем логические слоты
	disks = assignLogicalSlots(disks)

	printInfo(fmt.Sprintf("Detected %d disks in physical slots", len(disks)))

	// Показываем итоговый список для отладки
	if debugMode {
		printDebug("Final disk list:")
		for _, disk := range disks {
			printDebug(fmt.Sprintf("  Slot %d: %s -> %s (%s, %dGB, %s, %d°C)",
				disk.LogicalSlot, disk.PhysicalSlot, disk.Device, disk.Type, disk.SizeGB, disk.SmartStatus, disk.Temperature))
		}
	}

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
	Rota       *bool         `json:"rota"`
	RM         *bool         `json:"rm"`
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

	// Улучшенное определение типа и интерфейса
	// Сначала проверяем имя устройства на NVMe
	if strings.Contains(device.Kname, "nvme") {
		disk.Type = "NVMe"
		disk.Interface = "NVMe"
	} else if device.Tran != nil {
		// Затем определяем по transport
		switch strings.ToLower(*device.Tran) {
		case "nvme":
			disk.Interface = "NVMe"
			disk.Type = "NVMe"
		case "sata":
			disk.Interface = "SATA"
		case "usb":
			disk.Interface = "USB"
			disk.Type = "USB"
			disk.IsRemovable = true
			disk.SmartStatus = "N/A" // Явно устанавливаем N/A для USB
		case "scsi":
			disk.Interface = "SCSI"
		default:
			disk.Interface = strings.ToUpper(*device.Tran)
		}
	}

	// Определяем removable устройства
	if device.RM != nil && *device.RM {
		disk.IsRemovable = true
		disk.SmartStatus = "N/A" // Явно устанавливаем N/A для removable
		if disk.Type == "" || disk.Type == "Unknown" {
			disk.Type = "USB"
			disk.Interface = "USB"
		}
	}

	// Определяем тип диска по rotation только если тип еще не определен
	if disk.Type == "" || disk.Type == "Unknown" {
		if device.Rota != nil && !*device.Rota {
			disk.Type = "SSD"
		} else if device.Rota != nil && *device.Rota {
			disk.Type = "HDD"
		}
	}

	// Финальная проверка
	if disk.Type == "" {
		disk.Type = "Unknown"
	}

	// Убеждаемся, что для USB устройств SMART = N/A
	if disk.Type == "USB" || disk.Interface == "USB" || disk.IsRemovable {
		disk.SmartStatus = "N/A"
	}

	if device.Mountpoint != nil && *device.Mountpoint != "" {
		disk.MountPoints = append(disk.MountPoints, *device.Mountpoint)
	}

	for _, child := range device.Children {
		if child.Mountpoint != nil && *child.Mountpoint != "" {
			disk.MountPoints = append(disk.MountPoints, *child.Mountpoint)
		}
	}

	if debugMode {
		printDebug(fmt.Sprintf("Parsed lsblk device %s: Type=%s, Interface=%s, Removable=%t, SMART=%s, Transport=%v",
			device.Kname, disk.Type, disk.Interface, disk.IsRemovable, disk.SmartStatus,
			func() string {
				if device.Tran != nil {
					return *device.Tran
				} else {
					return "nil"
				}
			}()))
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

// ===== FALLBACK ФУНКЦИИ =====

func getDisksFromByPath() ([]DiskInfo, error) {
	printDebug("Trying by-path detection method...")

	byPathDir := "/dev/disk/by-path"
	if _, err := os.Stat(byPathDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("by-path directory not found")
	}

	entries, err := os.ReadDir(byPathDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read by-path directory: %v", err)
	}

	var disks []DiskInfo
	processedDevices := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Пропускаем разделы
		if isPartitionByPathName(entry.Name()) {
			continue
		}

		pathLink := filepath.Join(byPathDir, entry.Name())
		realDevice, err := filepath.EvalSymlinks(pathLink)
		if err != nil {
			printDebug(fmt.Sprintf("Failed to resolve symlink %s: %v", pathLink, err))
			continue
		}

		if processedDevices[realDevice] {
			continue
		}

		if !isBlockDevice(realDevice) {
			continue
		}

		disk := DiskInfo{
			Device:     realDevice,
			ByPathPath: pathLink,
		}

		// Предварительное определение типа по пути
		disk.Type = determineTypeFromPath(pathLink)
		disk.Interface = determineInterfaceFromPath(pathLink)
		if disk.Type == "USB" {
			disk.IsRemovable = true
		}

		disk = enrichDiskInfo(disk)
		disk.PhysicalSlot = extractPhysicalSlotFromPath(pathLink)
		disk.CleanID = cleanIdentifier(disk.ByIdPath)

		disks = append(disks, disk)
		processedDevices[realDevice] = true

		printDebug(fmt.Sprintf("Found disk via by-path: %s -> %s (Type: %s)", pathLink, realDevice, disk.Type))
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disks found via by-path")
	}

	printDebug(fmt.Sprintf("Found %d disks via by-path", len(disks)))
	return disks, nil
}

// determineTypeFromPath определяет тип диска по пути by-path
func determineTypeFromPath(pathLink string) string {
	filename := filepath.Base(pathLink)

	if strings.Contains(filename, "-nvme-") {
		return "NVMe"
	}
	if strings.Contains(filename, "-usb-") {
		return "USB"
	}
	if strings.Contains(filename, "-ata-") {
		return "Unknown" // Определим точнее через sysfs
	}

	return "Unknown"
}

// determineInterfaceFromPath определяет интерфейс по пути by-path
func determineInterfaceFromPath(pathLink string) string {
	filename := filepath.Base(pathLink)

	if strings.Contains(filename, "-nvme-") {
		return "NVMe"
	}
	if strings.Contains(filename, "-usb-") {
		return "USB"
	}
	if strings.Contains(filename, "-ata-") {
		return "SATA"
	}
	if strings.Contains(filename, "-scsi-") {
		return "SCSI"
	}

	return "Unknown"
}

func getDisksFromProcPartitions() ([]DiskInfo, error) {
	printDebug("Trying /proc/partitions detection method...")

	file, err := os.Open("/proc/partitions")
	if err != nil {
		return nil, fmt.Errorf("failed to open /proc/partitions: %v", err)
	}
	defer file.Close()

	var disks []DiskInfo
	scanner := bufio.NewScanner(file)

	// Пропускаем заголовок
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "major") || strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		deviceName := fields[3]

		// Пропускаем разделы и другие не-диски
		if isPartitionName(deviceName) {
			continue
		}

		// Игнорируем loop, ram, dm-* устройства
		if strings.HasPrefix(deviceName, "loop") ||
			strings.HasPrefix(deviceName, "ram") ||
			strings.HasPrefix(deviceName, "dm-") {
			continue
		}

		device := "/dev/" + deviceName

		// Проверяем, что устройство действительно существует
		if _, err := os.Stat(device); os.IsNotExist(err) {
			continue
		}

		// Парсим размер в килобайтах
		sizeKB, err := strconv.Atoi(fields[2])
		if err != nil {
			sizeKB = 0
		}
		sizeGB := sizeKB / 1024 / 1024

		disk := DiskInfo{
			Device: device,
			SizeGB: sizeGB,
		}

		// Предварительное определение типа по имени устройства
		disk.Type = determineTypeFromDeviceName(deviceName)
		disk.Interface = determineInterfaceFromDeviceName(deviceName)

		disk = enrichDiskInfo(disk)
		disk = determinePhysicalSlot(disk)
		disk.CleanID = cleanIdentifier(disk.ByIdPath)

		disks = append(disks, disk)
		printDebug(fmt.Sprintf("Found disk via /proc/partitions: %s (%dGB, Type: %s)", device, sizeGB, disk.Type))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading /proc/partitions: %v", err)
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disks found in /proc/partitions")
	}

	printDebug(fmt.Sprintf("Found %d disks via /proc/partitions", len(disks)))
	return disks, nil
}

// determineTypeFromDeviceName определяет тип диска по имени устройства
func determineTypeFromDeviceName(deviceName string) string {
	if strings.HasPrefix(deviceName, "nvme") {
		return "NVMe"
	}
	if strings.HasPrefix(deviceName, "sd") {
		return "Unknown" // Может быть SATA, USB, SCSI - определим точнее
	}
	if strings.HasPrefix(deviceName, "vd") {
		return "VirtIO"
	}
	if strings.HasPrefix(deviceName, "hd") {
		return "IDE"
	}
	if strings.HasPrefix(deviceName, "mmcblk") {
		return "MMC"
	}
	return "Unknown"
}

// determineInterfaceFromDeviceName определяет интерфейс по имени устройства
func determineInterfaceFromDeviceName(deviceName string) string {
	if strings.HasPrefix(deviceName, "nvme") {
		return "NVMe"
	}
	if strings.HasPrefix(deviceName, "sd") {
		return "SCSI" // Может быть SATA, USB, SCSI - определим точнее через sysfs
	}
	if strings.HasPrefix(deviceName, "vd") {
		return "VirtIO"
	}
	if strings.HasPrefix(deviceName, "hd") {
		return "IDE"
	}
	if strings.HasPrefix(deviceName, "mmcblk") {
		return "MMC"
	}
	return "Unknown"
}

// ===== ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ ДЛЯ FALLBACK =====

func isPartitionByPathName(name string) bool {
	// Проверяем, является ли это разделом по имени by-path
	partitionPatterns := []string{"-part", "_part"}
	for _, pattern := range partitionPatterns {
		if strings.Contains(name, pattern) {
			return true
		}
	}

	// Проверяем номер раздела в конце
	if len(name) > 0 {
		lastChar := name[len(name)-1]
		if lastChar >= '1' && lastChar <= '9' {
			// Проверяем, что перед цифрой есть разделитель
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

func isPartitionName(deviceName string) bool {
	// Проверяем, является ли имя устройства разделом
	// sda1, sda2, nvme0n1p1, mmcblk0p1 и т.д.

	// Для sd*, vd*, hd* устройств - цифра в конце означает раздел
	if regexp.MustCompile(`^[sv]d[a-z]\d+$`).MatchString(deviceName) {
		return true
	}
	if regexp.MustCompile(`^hd[a-z]\d+$`).MatchString(deviceName) {
		return true
	}

	// Для nvme устройств - p[цифра] означает раздел
	if regexp.MustCompile(`^nvme\d+n\d+p\d+$`).MatchString(deviceName) {
		return true
	}

	// Для mmcblk устройств - p[цифра] означает раздел
	if regexp.MustCompile(`^mmcblk\d+p\d+$`).MatchString(deviceName) {
		return true
	}

	return false
}

func isBlockDevice(devicePath string) bool {
	info, err := os.Stat(devicePath)
	if err != nil {
		return false
	}

	// Проверяем, что это блочное устройство
	mode := info.Mode()
	return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
}

// ===== ОПРЕДЕЛЕНИЕ ФИЗИЧЕСКИХ СЛОТОВ =====

func determinePhysicalSlot(disk DiskInfo) DiskInfo {
	// Ищем by-path для определения физического слота
	if disk.ByPathPath == "" {
		disk.ByPathPath = findByPathForDevice(disk.Device)
	}

	if disk.ByIdPath == "" {
		disk.ByIdPath = findByIdForDevice(disk.Device)
	}

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

	// Ищем USB порты - улучшенная логика
	if strings.Contains(filename, "-usb-") {
		// Пытаемся извлечь номер порта из разных форматов
		// usb-0:1:1.0 -> порт 1
		// usb-0:2:1.0 -> порт 2
		patterns := []string{
			`-usb-\d+:(\d+):`,   // usb-0:1:1.0
			`-usb-[^-]+-(\d+):`, // другие форматы
			`usb(\d+)-`,         // usb1-, usb2-
		}

		for _, pattern := range patterns {
			re := regexp.MustCompile(pattern)
			if matches := re.FindStringSubmatch(filename); len(matches) > 1 {
				return fmt.Sprintf("USB%s", matches[1])
			}
		}

		// Если не смогли извлечь номер, используем USB0
		return "USB0"
	}

	// SCSI устройства
	if strings.Contains(filename, "-scsi-") {
		re := regexp.MustCompile(`-scsi-(\d+):`)
		if matches := re.FindStringSubmatch(filename); len(matches) > 1 {
			return fmt.Sprintf("SCSI%s", matches[1])
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
		maxSlot = len(disks) + 4 // Достаточно слотов для показа всех возможных
	}

	// Создаём массив слотов для всех возможных позиций
	slots := make([]DiskInfo, maxSlot+1) // +1 because slots start from 1

	// Создаем мапирование физических слотов к логическим из найденных дисков
	physToLogical := make(map[string]int)
	for _, disk := range disks {
		physToLogical[disk.PhysicalSlot] = disk.LogicalSlot
		if disk.LogicalSlot > 0 && disk.LogicalSlot <= maxSlot {
			slots[disk.LogicalSlot] = disk
		}
	}

	printInfo(fmt.Sprintf("Currently detected %d disks:", len(disks)))
	for _, disk := range disks {
		printInfo(fmt.Sprintf("  Slot %d (%s): %s", disk.LogicalSlot, disk.PhysicalSlot, disk.CleanID))
	}

	// Проверяем каждое требование
	allPassed := true
	for _, req := range config.SlotRequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		if !checkSlotRequirement(slots, req, config, disks) {
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

func checkSlotRequirement(slots []DiskInfo, req SlotRequirement, config *Config, _ []DiskInfo) bool {
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

	// Проверяем обязательные слоты - ОСНОВНАЯ ЛОГИКА
	missingSlots := []int{}
	for _, slotNum := range req.RequiredSlots {
		if slotNum > 0 && slotNum < len(slots) {
			disk := slots[slotNum]
			if disk.Device == "" {
				// Слот пустой!
				if !req.AllowEmpty {
					printError(fmt.Sprintf("  MISSING: Required slot %d is empty", slotNum))
					missingSlots = append(missingSlots, slotNum)
					reqPassed = false
				} else {
					printWarning(fmt.Sprintf("  Required slot %d is empty (allowed)", slotNum))
				}
			} else {
				// Слот занят - проверяем диск
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
		} else {
			printWarning(fmt.Sprintf("  Invalid slot number in requirement: %d", slotNum))
		}
	}

	// Показываем детали отсутствующих слотов
	if len(missingSlots) > 0 {
		printError(fmt.Sprintf("  Missing %d required slot(s): %v", len(missingSlots), missingSlots))
		printError("  These slots must contain disks for the system to function properly")
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
			} else {
				printInfo(fmt.Sprintf("    Optional slot %d: empty (allowed)", slotNum))
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
		maxSlots = len(disks) + 4
	}

	// Создаём данные слотов
	slotData := make([]DiskInfo, maxSlots+1)
	slotResults := make([]SlotCheckResult, maxSlots+1)

	// Заполняем слоты найденными дисками
	for _, disk := range disks {
		if disk.LogicalSlot > 0 && disk.LogicalSlot <= maxSlots {
			slotData[disk.LogicalSlot] = disk
			slotResults[disk.LogicalSlot] = checkSlotForVisualization(disk, config)
		}
	}

	// Определяем, какие слоты должны быть заняты согласно конфигурации
	requiredSlots := make(map[int]bool)
	for _, req := range config.SlotRequirements {
		for _, slotNum := range req.RequiredSlots {
			if slotNum > 0 && slotNum <= maxSlots {
				requiredSlots[slotNum] = true
			}
		}
	}

	// Проверяем пустые слоты и помечаем отсутствующие как ошибки
	hasErrors := false
	hasWarnings := false
	missingDisks := []int{}

	for i := 1; i <= maxSlots; i++ {
		if slotData[i].Device == "" {
			// Пустой слот
			if requiredSlots[i] {
				// Этот слот должен быть занят!
				slotResults[i] = SlotCheckResult{
					Status:  "error",
					HasDisk: false,
					Issues:  []string{"Required disk missing"},
				}
				missingDisks = append(missingDisks, i)
				hasErrors = true
			} else {
				// Пустой необязательный слот
				slotResults[i] = SlotCheckResult{
					Status:  "empty",
					HasDisk: false,
				}
			}
		} else {
			// Занятый слот - проверяем статус диска
			status := slotResults[i].Status
			if status == "error" {
				hasErrors = true
			} else if status == "warning" {
				hasWarnings = true
			}
		}
	}

	// Легенда с информацией об ошибках
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Disk Present & OK  ", ColorGreen, "████", ColorReset)
	fmt.Printf("  %s%s%s Disk with Issues  ", ColorYellow, "████", ColorReset)
	fmt.Printf("  %s%s%s MISSING Required Disk  ", ColorRed, "░░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Optional Slot\n", ColorWhite, "░░░░", ColorReset)
	fmt.Println()

	// Сообщаем об отсутствующих дисках
	if len(missingDisks) > 0 {
		printError(fmt.Sprintf("CRITICAL: %d required disk(s) missing!", len(missingDisks)))
		for _, slotNum := range missingDisks {
			printError(fmt.Sprintf("  - Slot %d: Required disk not found", slotNum))
		}
		fmt.Println()
	}

	// Рендерим визуализацию
	renderSlotVisualization(slotData, slotResults, config, maxSlots)

	// Финальный статус
	if hasErrors {
		printError("CRITICAL: Slot configuration validation FAILED!")
		printError("Some required disks are missing from their expected slots")
		return fmt.Errorf("critical: missing required disks in slots %v", missingDisks)
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

	// Находим требования, которые применимы к этому диску
	var applicableReqs []SlotRequirement
	for _, req := range config.SlotRequirements {
		// Проверяем, попадает ли диск под это требование
		if len(req.RequiredSlots) > 0 {
			for _, slotNum := range req.RequiredSlots {
				if disk.LogicalSlot == slotNum {
					applicableReqs = append(applicableReqs, req)
					break
				}
			}
		}
		if len(req.OptionalSlots) > 0 {
			for _, slotNum := range req.OptionalSlots {
				if disk.LogicalSlot == slotNum {
					applicableReqs = append(applicableReqs, req)
					break
				}
			}
		}
	}

	// Применяем проверки из найденных требований
	for _, req := range applicableReqs {
		// Проверка типа
		if req.RequiredType != "" && req.RequiredType != "any" && disk.Type != req.RequiredType {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Type mismatch: %s (required %s)", disk.Type, req.RequiredType))
			result.TypeOK = false
			result.Status = "error"
		}

		// Проверка размера - ЭТО БЫЛО ПРОПУЩЕНО!
		if req.MinSizeGB > 0 && disk.SizeGB < req.MinSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Size too small: %dGB (min %dGB)", disk.SizeGB, req.MinSizeGB))
			result.SizeOK = false
			result.Status = "error"
		}

		if req.MaxSizeGB > 0 && disk.SizeGB > req.MaxSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Size too large: %dGB (max %dGB)", disk.SizeGB, req.MaxSizeGB))
			result.SizeOK = false
			result.Status = "error"
		}

		// Проверка здоровья
		if config.CheckSmart && req.RequireHealth && shouldCheckSMART(disk) {
			if disk.SmartStatus == "FAILED" {
				result.Issues = append(result.Issues, "SMART health failed")
				result.HealthOK = false
				result.Status = "error"
			} else if disk.SmartStatus == "N/A" {
				result.Issues = append(result.Issues, "SMART unavailable")
				result.HealthWarn = true
				if result.Status == "ok" {
					result.Status = "warning"
				}
			}
		}

		// Проверка температуры
		if config.CheckTemp && req.MaxTempC > 0 && disk.Temperature > 0 {
			if disk.Temperature > req.MaxTempC {
				result.Issues = append(result.Issues,
					fmt.Sprintf("High temperature: %d°C (max %d°C)", disk.Temperature, req.MaxTempC))
				result.TempOK = false
				result.Status = "error"
			}
		}
	}

	// Общие проверки без требований (для случаев когда нет специфических требований)
	if len(applicableReqs) == 0 {
		// Основные проверки здоровья диска для USB устройств НЕ проверяем SMART
		if config.CheckSmart && shouldCheckSMART(disk) && disk.SmartStatus == "FAILED" {
			result.Issues = append(result.Issues, "SMART health failed")
			result.HealthOK = false
			result.Status = "error"
		} else if config.CheckSmart && shouldCheckSMART(disk) && disk.SmartStatus == "N/A" {
			result.Issues = append(result.Issues, "SMART unavailable")
			result.HealthWarn = true
			if result.Status == "ok" {
				result.Status = "warning"
			}
		}

		// Общая проверка температуры (если диск не USB и температура слишком высокая)
		if config.CheckTemp && disk.Temperature > 70 && shouldCheckSMART(disk) {
			result.Issues = append(result.Issues, fmt.Sprintf("High temperature: %d°C", disk.Temperature))
			result.TempOK = false
			result.Status = "error"
		}
	}

	// Дополнительная отладочная информация для USB устройств
	if debugMode && (disk.Type == "USB" || disk.IsRemovable) {
		printDebug(fmt.Sprintf("USB/Removable device %s: SMART check skipped, Type=%s, Interface=%s, Removable=%t",
			disk.Device, disk.Type, disk.Interface, disk.IsRemovable))
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
				// Красный цвет для отсутствующих обязательных дисков
				fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
			case "empty":
				// Серый цвет для пустых необязательных слотов
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
				// Для пустых слотов показываем статус
				switch result.Status {
				case "error":
					fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
				default:
					fmt.Print(strings.Repeat(" ", width))
				}
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
				// Для пустых слотов
				switch result.Status {
				case "error":
					fmt.Print(ColorRed + centerText("REQ", width) + ColorReset)
				default:
					fmt.Print(strings.Repeat(" ", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Ряд температуры (если включен)
		if config.Visualization.ShowTemp {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := rowStart + i
				result := slotResults[slotIdx]

				if slotData[slotIdx].Device != "" {
					// Используем обновленную функцию formatTemp с проверкой USB
					tempText := centerText(formatTemp(slotData[slotIdx].Temperature, slotData[slotIdx].Type, slotData[slotIdx].IsRemovable), width)

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
					// Для пустых слотов
					switch result.Status {
					case "error":
						fmt.Print(ColorRed + centerText("ERR", width) + ColorReset)
					default:
						fmt.Print(strings.Repeat(" ", width))
					}
				}
				fmt.Print("│")
			}
			fmt.Println()
		}

		// Ряд SMART статуса (если включен)
		if config.Visualization.ShowSmart {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := rowStart + i
				result := slotResults[slotIdx]

				if slotData[slotIdx].Device != "" {
					// Используем новую функцию formatSmart с проверкой USB
					smartText := centerText(formatSmart(slotData[slotIdx].SmartStatus, slotData[slotIdx].Type, slotData[slotIdx].IsRemovable), width)

					// Цветовое кодирование SMART статуса
					smartStatus := formatSmart(slotData[slotIdx].SmartStatus, slotData[slotIdx].Type, slotData[slotIdx].IsRemovable)
					if smartStatus == "N/A" {
						// Серый цвет для N/A (USB устройства)
						fmt.Print(ColorWhite + smartText + ColorReset)
					} else if smartStatus == "FAIL" || (result.Status == "error" && !result.HealthOK) {
						fmt.Print(ColorRed + smartText + ColorReset)
					} else if smartStatus == "OK" && result.Status == "ok" {
						fmt.Print(ColorGreen + smartText + ColorReset)
					} else if result.Status == "warning" || result.HealthWarn {
						fmt.Print(ColorYellow + smartText + ColorReset)
					} else {
						fmt.Print(smartText)
					}
				} else {
					// Для пустых слотов
					switch result.Status {
					case "error":
						fmt.Print(ColorRed + centerText("ERR", width) + ColorReset)
					default:
						fmt.Print(strings.Repeat(" ", width))
					}
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

	// Создаем фиксированное мапирование физических слотов на логические
	slotMapping := make(map[string]int)
	expectedSlots := make(map[int]bool) // Какие слоты должны быть заняты

	// Группируем диски по типам
	typeGroups := make(map[string][]DiskInfo)
	for _, disk := range disks {
		typeGroups[disk.Type] = append(typeGroups[disk.Type], disk)
	}

	var requirements []SlotRequirement
	typeVisuals := make(map[string]DiskVisual)

	// Показываем найденные диски и создаем мапирование
	for _, disk := range disks {
		slotMapping[disk.PhysicalSlot] = disk.LogicalSlot
		expectedSlots[disk.LogicalSlot] = true
		printInfo(fmt.Sprintf("  Slot %d (%s): %s (%s, %dGB, SMART: %s)",
			disk.LogicalSlot, disk.PhysicalSlot, disk.CleanID, disk.Type, disk.SizeGB, disk.SmartStatus))
	}

	// Проверяем SMART - исключаем USB устройства
	globalHasHealth := false
	smartCapableDevices := 0
	for _, disk := range disks {
		if shouldCheckSMART(disk) {
			smartCapableDevices++
			if disk.SmartStatus == "PASSED" {
				globalHasHealth = true
			}
		}
	}

	// Создаём требования по типам с конкретными слотами
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

		// Для USB устройств не требуем SMART проверки
		requireHealth := globalHasHealth && diskType != "USB" && shouldCheckSMART(disksOfType[0])

		req := SlotRequirement{
			Name:          fmt.Sprintf("%s slots (%d required)", diskType, len(disksOfType)),
			MinOccupied:   len(disksOfType),
			RequiredType:  diskType,
			MinSizeGB:     minSize,
			RequireHealth: requireHealth,
			MaxTempC:      70,
			RequiredSlots: requiredSlots, // Конкретные слоты, которые должны быть заняты
			AllowEmpty:    false,         // Не разрешаем пустые обязательные слоты
		}

		requirements = append(requirements, req)

		// Создаём визуальные элементы
		visual := generateDiskVisual(diskType)
		typeVisuals[diskType] = visual
	}

	// Создаем дополнительное требование для общей проверки слотов
	allRequiredSlots := make([]int, 0, len(expectedSlots))
	for slot := range expectedSlots {
		allRequiredSlots = append(allRequiredSlots, slot)
	}

	generalReq := SlotRequirement{
		Name:          fmt.Sprintf("All expected slots (%d total)", len(allRequiredSlots)),
		MinOccupied:   len(allRequiredSlots),
		RequiredType:  "any", // Любой тип подходит
		RequiredSlots: allRequiredSlots,
		AllowEmpty:    false,
	}
	requirements = append(requirements, generalReq)

	config := Config{
		SlotRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals: typeVisuals,
			TotalSlots:  len(disks) + 4, // Больше слотов для показа потенциально пустых
			SlotWidth:   12,
			SlotsPerRow: 6,
			ShowTemp:    true,
			ShowSmart:   smartCapableDevices > 0, // Показываем SMART только если есть устройства с поддержкой
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

	printSuccess("Configuration created successfully with fixed slot mapping")
	printInfo(fmt.Sprintf("Total slots configured: %d", len(disks)))
	printInfo("Configuration will detect missing disks in required slots")
	if globalHasHealth {
		printInfo(fmt.Sprintf("SMART checking enabled (%d SMART-capable devices)", smartCapableDevices))
	} else if smartCapableDevices > 0 {
		printWarning(fmt.Sprintf("SMART available but no healthy devices detected (%d SMART-capable devices)", smartCapableDevices))
	} else {
		printInfo("No SMART-capable devices found (USB devices don't support SMART)")
	}

	// Сохраняем информацию о физическом мапировании для отладки
	printInfo("Physical slot mapping:")
	for physSlot, logSlot := range slotMapping {
		printInfo(fmt.Sprintf("  %s -> Logical slot %d", physSlot, logSlot))
	}

	return nil
}

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

func enrichDiskInfo(disk DiskInfo) DiskInfo {
	// Получаем размер если не определён
	if disk.SizeGB == 0 {
		disk.SizeGB = getDiskSizeFromDevice(disk.Device)
	}

	// Обогащаем из sysfs
	disk = enrichFromSysBlock(disk)

	// Получаем точки монтирования
	disk.MountPoints = getMountPoints(disk.Device)

	// Дополнительная проверка на USB через by-path
	if disk.ByPathPath == "" {
		disk.ByPathPath = findByPathForDevice(disk.Device)
	}
	if strings.Contains(strings.ToLower(disk.ByPathPath), "usb") {
		disk.Type = "USB"
		disk.Interface = "USB"
		disk.SmartStatus = "N/A"
		disk.IsRemovable = true
	}

	// SMART данные - пропускаем для USB и других removable устройств
	if shouldCheckSMART(disk) {
		if debugMode {
			printDebug(fmt.Sprintf("Checking SMART for %s (Type: %s, Interface: %s, Removable: %t)",
				disk.Device, disk.Type, disk.Interface, disk.IsRemovable))
		}

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
	} else {
		// Явно устанавливаем N/A для USB и других removable устройств
		disk.SmartStatus = "N/A"
		if debugMode {
			printDebug(fmt.Sprintf("Skipping SMART for %s (Type: %s, Interface: %s, Removable: %t) - Setting SMART to N/A",
				disk.Device, disk.Type, disk.Interface, disk.IsRemovable))
		}
	}

	return disk
}

// shouldCheckSMART определяет, нужно ли проверять SMART для данного диска
func shouldCheckSMART(disk DiskInfo) bool {
	// Пропускаем SMART для USB устройств (по типу и интерфейсу)
	if disk.Type == "USB" || disk.Interface == "USB" || disk.IsRemovable {
		return false
	}

	// Пропускаем для виртуальных устройств
	if disk.Type == "VirtIO" || strings.Contains(disk.Device, "vd") {
		return false
	}

	// Пропускаем для MMC/SD карт
	if strings.Contains(disk.Device, "mmcblk") || disk.Type == "MMC" {
		return false
	}

	// Дополнительная проверка по пути - если в by-path есть USB, то точно USB
	if strings.Contains(strings.ToLower(disk.ByPathPath), "usb") {
		return false
	}

	// Проверяем SMART только для стационарных дисков
	return disk.Type == "SATA" || disk.Type == "NVMe" || disk.Type == "SSD" || disk.Type == "HDD" || disk.Interface == "SATA" || disk.Interface == "NVMe"
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
		if disk.Type == "" {
			disk.Type = "Unknown"
		}
		return disk
	}

	if model, err := readSysFile(sysPath + "/device/model"); err == nil && disk.Model == "" {
		disk.Model = strings.TrimSpace(model)
	}

	if vendor, err := readSysFile(sysPath + "/device/vendor"); err == nil && disk.Vendor == "" {
		disk.Vendor = strings.TrimSpace(vendor)
	}

	if serial, err := readSysFile(sysPath + "/device/serial"); err == nil && disk.Serial == "" {
		disk.Serial = strings.TrimSpace(serial)
	}

	// Проверяем removable СНАЧАЛА
	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		disk.IsRemovable = strings.TrimSpace(removable) == "1"
	}

	// Проверяем USB по device path в sysfs
	if devicePath, err := os.Readlink(sysPath + "/device"); err == nil {
		if strings.Contains(devicePath, "usb") {
			disk.IsRemovable = true
			disk.Type = "USB"
			disk.Interface = "USB"
		}
	}

	// Если removable и тип еще не определен как USB, то это USB
	if disk.IsRemovable && disk.Type != "USB" {
		disk.Type = "USB"
		disk.Interface = "USB"
	}

	// Определяем тип и интерфейс только если они еще не определены и это не USB
	if (disk.Type == "" || disk.Type == "Unknown") && !disk.IsRemovable && disk.Type != "USB" {
		disk.Type = determineDiskTypeFromSys(deviceName, sysPath)
	}

	if disk.Interface == "" && !disk.IsRemovable && disk.Type != "USB" {
		disk.Interface = determineInterface(deviceName, sysPath)
	}

	// Получаем rotation speed только для HDD (не USB)
	if disk.Type == "HDD" && !disk.IsRemovable {
		if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
			if strings.TrimSpace(rotational) == "1" {
				if rate, err := readSysFile(sysPath + "/device/rotation_rate"); err == nil {
					if rpm, parseErr := strconv.Atoi(strings.TrimSpace(rate)); parseErr == nil && rpm > 0 {
						disk.RotSpeed = rpm
					}
				}
			}
		}
	}

	if debugMode {
		printDebug(fmt.Sprintf("Enriched from sysfs %s: Type=%s, Interface=%s, Removable=%t",
			deviceName, disk.Type, disk.Interface, disk.IsRemovable))
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
	// Проверяем NVMe по имени устройства
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	// Проверяем USB устройства
	if devicePath, err := os.Readlink(sysPath + "/device"); err == nil {
		if strings.Contains(devicePath, "usb") {
			return "USB"
		}
	}

	// Проверяем removable устройства
	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		if strings.TrimSpace(removable) == "1" {
			return "USB"
		}
	}

	// Проверяем rotation только для не-USB устройств
	if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
		if strings.TrimSpace(rotational) == "0" {
			return "SSD"
		} else {
			return "HDD"
		}
	}

	return "Unknown"
}

func formatSmart(smartStatus string, diskType string, isRemovable bool) string {
	// Для USB и removable устройств всегда показываем N/A
	if diskType == "USB" || isRemovable || smartStatus == "N/A" {
		return "N/A"
	}

	// Для других устройств показываем реальный статус или ? если не определен
	if smartStatus == "" {
		return "?"
	}

	// Сокращаем длинные статусы для помещения в ячейку
	switch strings.ToUpper(smartStatus) {
	case "PASSED":
		return "OK"
	case "FAILED":
		return "FAIL"
	default:
		return smartStatus
	}
}

func determineInterface(deviceName, sysPath string) string {
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	// Проверяем путь к устройству в sysfs
	if devicePath, err := os.Readlink(sysPath + "/device"); err == nil {
		if strings.Contains(devicePath, "usb") {
			return "USB"
		}
		if strings.Contains(devicePath, "ata") {
			return "SATA"
		}
		if strings.Contains(devicePath, "nvme") {
			return "NVMe"
		}
		if strings.Contains(devicePath, "scsi") {
			return "SCSI"
		}
		if strings.Contains(devicePath, "virtio") {
			return "VirtIO"
		}
	}

	// Fallback по префиксу имени устройства
	if strings.HasPrefix(deviceName, "sd") {
		return "SATA" // Чаще всего это SATA
	}
	if strings.HasPrefix(deviceName, "nvme") {
		return "NVMe"
	}
	if strings.HasPrefix(deviceName, "vd") {
		return "VirtIO"
	}
	if strings.HasPrefix(deviceName, "hd") {
		return "IDE"
	}
	if strings.HasPrefix(deviceName, "mmcblk") {
		return "MMC"
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

		// Проверяем общее состояние SMART
		if strings.Contains(line, "SMART overall-health") {
			if strings.Contains(line, "PASSED") {
				smart.Status = "PASSED"
			} else if strings.Contains(line, "FAILED") {
				smart.Status = "FAILED"
			}
		}

		// Парсим температуру для разных типов дисков
		if temp := parseTemperature(line); temp > 0 {
			smart.Temperature = temp
		}

		// Power-on hours
		if strings.Contains(line, "Power_On_Hours") {
			smart.PowerOnHours = extractSMARTValue(line)
		}

		// Power cycle count
		if strings.Contains(line, "Power_Cycle_Count") {
			smart.PowerCycles = extractSMARTValue(line)
		}

		// Firmware version
		if strings.Contains(line, "Firmware Version:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				smart.Firmware = strings.TrimSpace(parts[1])
			}
		}

		// Rotation rate
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

// parseTemperature улучшенная функция для парсинга температуры
func parseTemperature(line string) int {
	lineLower := strings.ToLower(line)

	// Для NVMe дисков
	if strings.Contains(lineLower, "temperature:") && !strings.Contains(lineLower, "sensor") {
		// Ищем "Temperature: 45 Celsius" или "Temperature: 45 C"
		re := regexp.MustCompile(`temperature:\s*(\d+)\s*(celsius|c)?`)
		if matches := re.FindStringSubmatch(lineLower); len(matches) >= 2 {
			if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 200 {
				return temp
			}
		}
	}

	// Для NVMe температурных сенсоров
	if strings.Contains(lineLower, "temperature sensor") && strings.Contains(lineLower, "celsius") {
		// "Temperature Sensor 1: 45 Celsius"
		re := regexp.MustCompile(`temperature sensor \d+:\s*(\d+)\s*celsius`)
		if matches := re.FindStringSubmatch(lineLower); len(matches) >= 2 {
			if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 200 {
				return temp
			}
		}
	}

	// Для SATA дисков - ищем атрибут Temperature_Celsius
	if strings.Contains(line, "Temperature_Celsius") {
		// Формат: "194 Temperature_Celsius     0x0022   100   100   000    Old_age   Always       -       28 (Min/Max 21/42)"
		// Температура обычно после последнего тире
		parts := strings.Split(line, "-")
		if len(parts) > 1 {
			lastPart := strings.TrimSpace(parts[len(parts)-1])
			// Извлекаем первое число из последней части
			re := regexp.MustCompile(`^(\d+)`)
			if matches := re.FindStringSubmatch(lastPart); len(matches) >= 2 {
				if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 200 {
					return temp
				}
			}
		}
	}

	// Другие форматы температуры
	if strings.Contains(lineLower, "current drive temperature") {
		re := regexp.MustCompile(`(\d+)\s*(degrees|c|celsius)?`)
		if matches := re.FindStringSubmatch(lineLower); len(matches) >= 2 {
			if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 200 {
				return temp
			}
		}
	}

	return 0
}

// extractSMARTValue извлекает значение из строки SMART атрибута
func extractSMARTValue(line string) int {
	// Для строк вида: "  9 Power_On_Hours          0x0032   099   099   000    Old_age   Always       -       1234"
	// Нужно взять последнее число после тире
	parts := strings.Split(line, "-")
	if len(parts) > 1 {
		lastPart := strings.TrimSpace(parts[len(parts)-1])
		// Извлекаем первое число
		re := regexp.MustCompile(`(\d+)`)
		if matches := re.FindStringSubmatch(lastPart); len(matches) >= 2 {
			if value, err := strconv.Atoi(matches[1]); err == nil {
				return value
			}
		}
	}
	return 0
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

func formatTemp(temp int, diskType string, isRemovable bool) string {
	// Для USB и removable устройств показываем N/A
	if diskType == "USB" || isRemovable {
		return "N/A"
	}

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
				fmt.Printf("  Temperature: %s\n", formatTemp(disk.Temperature, disk.Type, disk.IsRemovable))
				fmt.Printf("  SMART Status: %s\n", formatSmart(disk.SmartStatus, disk.Type, disk.IsRemovable))
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
