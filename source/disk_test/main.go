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
	"strconv"
	"strings"
)

const VERSION = "1.0.1"

type DiskInfo struct {
	Device      string   `json:"device"`       // Current device path (e.g., /dev/sda)
	StableID    string   `json:"stable_id"`    // Stable identifier (from by-id)
	WWN         string   `json:"wwn"`          // World Wide Name if available
	Model       string   `json:"model"`        // Disk model
	Serial      string   `json:"serial"`       // Serial number
	SizeGB      int      `json:"size_gb"`      // Size in GB
	Type        string   `json:"type"`         // HDD, SSD, NVMe
	Interface   string   `json:"interface"`    // SATA, NVMe, USB, etc.
	Vendor      string   `json:"vendor"`       // Manufacturer
	Firmware    string   `json:"firmware"`     // Firmware version
	RotSpeed    int      `json:"rotation_rpm"` // Rotation speed for HDDs (0 for SSDs)
	Temperature int      `json:"temperature"`  // Temperature in Celsius
	SmartStatus string   `json:"smart_status"` // SMART overall health (PASSED/FAILED/N/A)
	PowerOnHrs  int      `json:"power_on_hrs"` // Power-on hours
	PowerCycles int      `json:"power_cycles"` // Power cycle count
	Slot        string   `json:"slot"`         // Physical slot identifier
	IsRemovable bool     `json:"is_removable"` // USB/external drives
	MountPoints []string `json:"mount_points"` // Current mount points
	ByIdPath    string   `json:"by_id_path"`   // Path in /dev/disk/by-id/
	ByPathPath  string   `json:"by_path_path"` // Path in /dev/disk/by-path/
}

type DiskRequirement struct {
	Name           string   `json:"name"`
	MinCount       int      `json:"min_count"`
	MaxCount       int      `json:"max_count"`
	RequiredType   string   `json:"required_type"`    // HDD, SSD, NVMe, any
	RequiredSlots  []string `json:"required_slots"`   // Specific stable IDs that must be populated
	MinSizeGB      int      `json:"min_size_gb"`      // Minimum size
	MaxSizeGB      int      `json:"max_size_gb"`      // Maximum size
	RequireHealth  bool     `json:"require_health"`   // Require SMART health OK
	MaxTempC       int      `json:"max_temp_c"`       // Maximum temperature
	AllowRemovable bool     `json:"allow_removable"`  // Allow USB/removable drives
	RequiredModel  string   `json:"required_model"`   // Specific model requirement
	MaxPowerOnHrs  int      `json:"max_power_on_hrs"` // Maximum power-on hours
	StableIDs      []string `json:"stable_ids"`       // Specific stable identifiers
}

type DiskVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type VisualizationConfig struct {
	TypeVisuals    map[string]DiskVisual `json:"type_visuals"`      // disk type -> visual
	StableIDToSlot map[string]int        `json:"stable_id_to_slot"` // stable_id -> logical slot
	TotalSlots     int                   `json:"total_slots"`       // Total slots to display
	SlotWidth      int                   `json:"slot_width"`        // Width of each slot
	SlotsPerRow    int                   `json:"slots_per_row"`     // Slots per row for multi-row layout
	ShowTemp       bool                  `json:"show_temperature"`  // Show temperature row
	ShowSmart      bool                  `json:"show_smart"`        // Show SMART status row
}

type Config struct {
	DiskRequirements []DiskRequirement   `json:"disk_requirements"`
	Visualization    VisualizationConfig `json:"visualization"`
	CheckSmart       bool                `json:"check_smart"`
	CheckTemp        bool                `json:"check_temperature"`
	SmartTimeout     int                 `json:"smart_timeout_seconds"`
}

type DiskCheckResult struct {
	Status     string // "ok", "warning", "error", "missing"
	Issues     []string
	SizeOK     bool
	TypeOK     bool
	HealthOK   bool
	TempOK     bool
	ModelOK    bool
	SizeWarn   bool
	TypeWarn   bool
	HealthWarn bool
	TempWarn   bool
	ModelWarn  bool
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

func getDiskInfo() ([]DiskInfo, error) {
	var disks []DiskInfo

	// Get disk information using stable identifiers from /dev/disk/by-id/
	stableDisks, err := getStableDiskIdentifiers()
	if err != nil {
		// Fallback to /proc/partitions if by-id is not available
		printWarning("Failed to read stable identifiers, falling back to /proc/partitions")
		return getDisksFromProcPartitions()
	}

	if debugMode {
		printDebug(fmt.Sprintf("Found %d stable disk identifiers", len(stableDisks)))
	}

	// Process each stable disk identifier
	for _, stableDisk := range stableDisks {
		disk := DiskInfo{
			StableID:   stableDisk.StableID,
			Device:     stableDisk.Device,
			ByIdPath:   stableDisk.ByIdPath,
			ByPathPath: stableDisk.ByPathPath,
			Serial:     stableDisk.Serial,
			WWN:        stableDisk.WWN,
		}

		// Get size from device
		disk.SizeGB = getDiskSizeFromDevice(disk.Device)

		// Get additional information from /sys/block/
		disk = enrichFromSysBlock(disk)

		// Get mount points
		disk.MountPoints = getMountPoints(disk.Device)

		// Generate slot name based on stable ID
		disk.Slot = generateSlotFromStableID(disk.StableID, disk.Type)

		// Enrich with SMART data
		disk = enrichDiskInfo(disk)

		disks = append(disks, disk)

		if debugMode {
			printDebug(fmt.Sprintf("Found stable disk: %s -> %s (%s, %dGB, %s)",
				disk.StableID, disk.Device, disk.Type, disk.SizeGB, disk.Interface))
		}
	}

	return disks, nil
}

type StableDiskInfo struct {
	StableID   string
	Device     string
	ByIdPath   string
	ByPathPath string
	Serial     string
	WWN        string
}

func getStableDiskIdentifiers() ([]StableDiskInfo, error) {
	var stableDisks []StableDiskInfo

	// Read /dev/disk/by-id/ directory
	byIdDir := "/dev/disk/by-id"
	entries, err := os.ReadDir(byIdDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %v", byIdDir, err)
	}

	processedDevices := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		idPath := filepath.Join(byIdDir, entry.Name())

		// Skip partitions - look for whole disks only
		if isPartitionByIdName(entry.Name()) {
			continue
		}

		// Resolve symlink to real device
		realDevice, err := filepath.EvalSymlinks(idPath)
		if err != nil {
			if debugMode {
				printDebug(fmt.Sprintf("Failed to resolve %s: %v", idPath, err))
			}
			continue
		}

		// Skip if we already processed this device
		if processedDevices[realDevice] {
			continue
		}
		processedDevices[realDevice] = true

		// Extract stable ID and additional info
		stableID := extractStableID(entry.Name())
		serial := extractSerialFromByIdName(entry.Name())
		wwn := extractWWNFromByIdName(entry.Name())

		// Get by-path if available
		byPathPath := findByPathForDevice(realDevice)

		stableDisk := StableDiskInfo{
			StableID:   stableID,
			Device:     realDevice,
			ByIdPath:   idPath,
			ByPathPath: byPathPath,
			Serial:     serial,
			WWN:        wwn,
		}

		stableDisks = append(stableDisks, stableDisk)

		if debugMode {
			printDebug(fmt.Sprintf("Stable disk found: %s -> %s (Serial: %s, WWN: %s)",
				stableID, realDevice, serial, wwn))
		}
	}

	return stableDisks, nil
}

func isPartitionByIdName(name string) bool {
	// Partition patterns in /dev/disk/by-id/:
	// - ata-DISK-MODEL_SERIAL-partN
	// - nvme-DISK-MODEL_SERIAL-partN
	// - scsi-SDISK-MODEL_SERIAL-partN
	// - usb-DISK-MODEL_SERIAL-partN

	partitionPatterns := []string{"-part", "_part"}
	for _, pattern := range partitionPatterns {
		if strings.Contains(name, pattern) {
			return true
		}
	}

	// Check for numbered endings that indicate partitions
	if len(name) > 0 {
		lastChar := name[len(name)-1]
		if lastChar >= '1' && lastChar <= '9' {
			// Additional check for numbered partitions
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

func extractStableID(byIdName string) string {
	// Extract a stable identifier from by-id name
	// Examples:
	// ata-SAMSUNG_SSD_860_EVO_500GB_S3Z2NB0M123456 -> SAMSUNG_SSD_860_EVO_500GB_S3Z2NB0M123456
	// nvme-Samsung_SSD_970_EVO_500GB_S466NB0M123456 -> Samsung_SSD_970_EVO_500GB_S466NB0M123456

	prefixes := []string{"ata-", "nvme-", "scsi-", "usb-", "wwn-"}

	stableID := byIdName
	for _, prefix := range prefixes {
		if strings.HasPrefix(byIdName, prefix) {
			stableID = strings.TrimPrefix(byIdName, prefix)
			break
		}
	}

	return stableID
}

func extractSerialFromByIdName(byIdName string) string {
	// Try to extract serial number from the by-id name
	if strings.Contains(byIdName, "wwn-") {
		return "" // WWN entries don't contain serial in readable form
	}

	// Remove prefix
	name := extractStableID(byIdName)

	// Serial is often the last part after underscore or dash
	parts := strings.FieldsFunc(name, func(c rune) bool {
		return c == '_' || c == '-'
	})

	if len(parts) > 0 {
		lastPart := parts[len(parts)-1]
		// Check if it looks like a serial (alphanumeric, reasonable length)
		if len(lastPart) >= 6 && len(lastPart) <= 40 {
			return lastPart
		}
	}

	return ""
}

func extractWWNFromByIdName(byIdName string) string {
	// Extract WWN if present
	if strings.HasPrefix(byIdName, "wwn-") {
		return strings.TrimPrefix(byIdName, "wwn-")
	}
	return ""
}

func findByPathForDevice(device string) string {
	// Find corresponding by-path entry for this device
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

func getDiskSizeFromDevice(device string) int {
	// Get disk size from /sys/block/*/size (in 512-byte sectors)
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

	// Convert 512-byte sectors to GB
	bytes := sectors * 512
	gb := bytes / 1024 / 1024 / 1024

	return int(gb)
}

func generateSlotFromStableID(stableID, diskType string) string {
	// Generate slot name based on stable ID and type
	// Extract model/vendor information for more readable slot names
	parts := strings.FieldsFunc(stableID, func(c rune) bool {
		return c == '_' || c == '-'
	})

	if len(parts) >= 2 {
		vendor := parts[0]
		model := parts[1]

		// Create a readable slot name
		return fmt.Sprintf("%s_%s_%s", diskType, vendor, model)
	}

	// Fallback to type + truncated stable ID
	if len(stableID) > 20 {
		return fmt.Sprintf("%s_%s", diskType, stableID[:20])
	}

	return fmt.Sprintf("%s_%s", diskType, stableID)
}

// Fallback function for systems without stable identifiers
func getDisksFromProcPartitions() ([]DiskInfo, error) {
	partitions, err := readProcPartitions()
	if err != nil {
		return nil, fmt.Errorf("failed to read /proc/partitions: %v", err)
	}

	var disks []DiskInfo
	for _, partition := range partitions {
		if isWholeDisk(partition.Name) {
			disk := DiskInfo{
				Device:   "/dev/" + partition.Name,
				SizeGB:   int(partition.SizeKB / 1024 / 1024), // Convert KB to GB
				StableID: partition.Name,                      // Use device name as fallback stable ID
				Slot:     generateSlotName(partition.Name),
			}

			// Get additional information from /sys/block/
			disk = enrichFromSysBlock(disk)

			// Get mount points
			disk.MountPoints = getMountPoints(disk.Device)

			// Enrich with SMART data
			disk = enrichDiskInfo(disk)

			disks = append(disks, disk)
		}
	}

	return disks, nil
}

type ProcPartition struct {
	Major  int
	Minor  int
	SizeKB int64
	Name   string
}

func readProcPartitions() ([]ProcPartition, error) {
	file, err := os.Open("/proc/partitions")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var partitions []ProcPartition
	scanner := bufio.NewScanner(file)

	// Skip header lines
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "major") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		major, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		minor, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		sizeKB, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}

		name := fields[3]

		partitions = append(partitions, ProcPartition{
			Major:  major,
			Minor:  minor,
			SizeKB: sizeKB,
			Name:   name,
		})
	}

	return partitions, scanner.Err()
}

func isWholeDisk(deviceName string) bool {
	// Check if this is a whole disk device, not a partition
	// NVMe devices: nvme0n1 is whole disk, nvme0n1p1 is partition
	if strings.Contains(deviceName, "nvme") {
		return !strings.Contains(deviceName, "p")
	}

	// SCSI/SATA devices: sda is whole disk, sda1 is partition
	if strings.HasPrefix(deviceName, "sd") {
		return len(deviceName) == 3 // sda, sdb, etc.
	}

	// MMC devices: mmcblk0 is whole disk, mmcblk0p1 is partition
	if strings.HasPrefix(deviceName, "mmcblk") {
		return !strings.Contains(deviceName, "p")
	}

	// VirtIO devices: vda is whole disk, vda1 is partition
	if strings.HasPrefix(deviceName, "vd") {
		return len(deviceName) == 3 // vda, vdb, etc.
	}

	return true
}

func enrichFromSysBlock(disk DiskInfo) DiskInfo {
	deviceName := strings.TrimPrefix(disk.Device, "/dev/")
	sysPath := fmt.Sprintf("/sys/block/%s", deviceName)

	// Check if device exists in sysfs
	if _, err := os.Stat(sysPath); os.IsNotExist(err) {
		disk.Type = "Unknown"
		return disk
	}

	// Read model
	if model, err := readSysFile(sysPath + "/device/model"); err == nil {
		disk.Model = strings.TrimSpace(model)
	}

	// Read vendor
	if vendor, err := readSysFile(sysPath + "/device/vendor"); err == nil {
		disk.Vendor = strings.TrimSpace(vendor)
	}

	// Read serial (if available)
	if serial, err := readSysFile(sysPath + "/device/serial"); err == nil {
		if disk.Serial == "" { // Don't override serial from by-id
			disk.Serial = strings.TrimSpace(serial)
		}
	}

	// Determine disk type and interface
	disk.Type = determineDiskTypeFromSys(deviceName, sysPath)
	disk.Interface = determineInterface(deviceName, sysPath)

	// Check if removable
	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		disk.IsRemovable = strings.TrimSpace(removable) == "1"
	}

	// Get rotation rate (for HDDs)
	if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
		if strings.TrimSpace(rotational) == "1" {
			// Try to get actual rotation rate
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
	// NVMe detection
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	// Check if rotational (HDD vs SSD)
	if rotational, err := readSysFile(sysPath + "/queue/rotational"); err == nil {
		if strings.TrimSpace(rotational) == "0" {
			return "SSD"
		} else {
			return "HDD"
		}
	}

	// Check removable media
	if removable, err := readSysFile(sysPath + "/removable"); err == nil {
		if strings.TrimSpace(removable) == "1" {
			return "USB"
		}
	}

	return "Unknown"
}

func determineInterface(deviceName, sysPath string) string {
	// NVMe interface
	if strings.Contains(deviceName, "nvme") {
		return "NVMe"
	}

	// Try to read from device path
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

	// SCSI/SATA devices
	if strings.HasPrefix(deviceName, "sd") {
		return "SATA"
	}

	return "Unknown"
}

func getMountPoints(device string) []string {
	var mountPoints []string

	// Read /proc/mounts
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

			// Check if this mount point belongs to our device or its partitions
			if strings.HasPrefix(mountedDevice, device) {
				mountPoints = append(mountPoints, mountPoint)
			}
		}
	}

	return mountPoints
}

func generateSlotName(deviceName string) string {
	// Generate logical slot names based on device names (fallback function)
	if strings.HasPrefix(deviceName, "nvme") {
		// Extract number from nvme0n1 format
		re := regexp.MustCompile(`nvme(\d+)`)
		if matches := re.FindStringSubmatch(deviceName); len(matches) > 1 {
			return fmt.Sprintf("NVMe%s", matches[1])
		}
		return "NVMe1"
	}

	if strings.HasPrefix(deviceName, "sd") {
		// Convert sda, sdb, etc. to SATA1, SATA2, etc.
		if len(deviceName) >= 3 {
			letter := deviceName[2]
			slotNum := int(letter-'a') + 1
			return fmt.Sprintf("SATA%d", slotNum)
		}
	}

	if strings.HasPrefix(deviceName, "vd") {
		// VirtIO disks: vda -> VirtIO1, etc.
		if len(deviceName) >= 3 {
			letter := deviceName[2]
			slotNum := int(letter-'a') + 1
			return fmt.Sprintf("VirtIO%d", slotNum)
		}
	}

	if strings.HasPrefix(deviceName, "mmcblk") {
		// MMC devices: mmcblk0 -> MMC1, etc.
		re := regexp.MustCompile(`mmcblk(\d+)`)
		if matches := re.FindStringSubmatch(deviceName); len(matches) > 1 {
			slotNum, _ := strconv.Atoi(matches[1])
			return fmt.Sprintf("MMC%d", slotNum+1)
		}
		return "MMC1"
	}

	return strings.ToUpper(deviceName)
}

func enrichDiskInfo(disk DiskInfo) DiskInfo {
	// Enrich with SMART data
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

	// Check if smartctl is available
	if _, err := exec.LookPath("smartctl"); err != nil {
		return smart, fmt.Errorf("smartctl not found")
	}

	// Run smartctl
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

	// Parse SMART output
	smart = parseSmartOutput(outputStr)
	return smart, nil
}

func parseSmartOutput(output string) SmartData {
	var smart SmartData
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Overall health assessment
		if strings.Contains(line, "SMART overall-health") {
			if strings.Contains(line, "PASSED") {
				smart.Status = "PASSED"
			} else if strings.Contains(line, "FAILED") {
				smart.Status = "FAILED"
			}
		}

		// Temperature
		if strings.Contains(line, "Temperature_Celsius") || strings.Contains(line, "Current Drive Temperature") {
			if temp := extractNumber(line); temp > 0 && temp < 200 {
				smart.Temperature = temp
			}
		}

		// Power-on hours
		if strings.Contains(line, "Power_On_Hours") {
			smart.PowerOnHours = extractNumber(line)
		}

		// Power cycle count
		if strings.Contains(line, "Power_Cycle_Count") {
			smart.PowerCycles = extractNumber(line)
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
				smart.RotationRate = 0 // SSD
			} else {
				smart.RotationRate = extractNumber(line)
			}
		}
	}

	return smart
}

func extractNumber(line string) int {
	// Extract the first reasonable number from a line
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

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for disk information to create configuration...")

	disks, err := getDiskInfo()
	if err != nil {
		return fmt.Errorf("could not scan disks: %v", err)
	}

	if len(disks) == 0 {
		return fmt.Errorf("no disks found - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d disk(s), creating configuration:", len(disks)))

	stableIDToSlot := make(map[string]int)
	typeVisuals := make(map[string]DiskVisual)

	// Group disks by type
	typeGroups := make(map[string][]DiskInfo)
	for _, disk := range disks {
		typeGroups[disk.Type] = append(typeGroups[disk.Type], disk)
	}

	var requirements []DiskRequirement

	// Create slot mapping using stable identifiers
	for i, disk := range disks {
		logicalSlot := i + 1
		stableIDToSlot[disk.StableID] = logicalSlot
		printInfo(fmt.Sprintf("  Mapping %s -> Slot %d (%s, %dGB, %s)",
			disk.StableID, logicalSlot, disk.Type, disk.SizeGB, disk.Slot))
		if disk.Serial != "" {
			printInfo(fmt.Sprintf("    Serial: %s", disk.Serial))
		}
		if disk.WWN != "" {
			printInfo(fmt.Sprintf("    WWN: %s", disk.WWN))
		}
	}

	// Check if any disk has SMART data available
	globalHasHealth := false

	// Create requirements and visuals by type
	for diskType, disksOfType := range typeGroups {
		printInfo(fmt.Sprintf("  Processing %d %s disk(s):", len(disksOfType), diskType))

		minSize := 0
		maxSize := 0
		hasHealth := false

		for _, disk := range disksOfType {
			printInfo(fmt.Sprintf("    - %s: %dGB (%s)", disk.StableID, disk.SizeGB, disk.Model))
			if disk.SmartStatus == "PASSED" {
				hasHealth = true
				globalHasHealth = true
			}

			if minSize == 0 || (disk.SizeGB > 0 && disk.SizeGB < minSize) {
				minSize = disk.SizeGB
			}
			if disk.SizeGB > maxSize {
				maxSize = disk.SizeGB
			}
		}

		// Create requirement for this type
		req := DiskRequirement{
			Name:          fmt.Sprintf("%s disks (%d found)", diskType, len(disksOfType)),
			MinCount:      len(disksOfType),
			RequiredType:  diskType,
			MinSizeGB:     minSize,
			RequireHealth: hasHealth,
			MaxTempC:      70, // Default temperature limit
		}

		// Add stable IDs for this type
		var stableIDs []string
		for _, disk := range disksOfType {
			stableIDs = append(stableIDs, disk.StableID)
		}
		req.StableIDs = stableIDs

		requirements = append(requirements, req)

		// Create visual for this type
		visual := generateDiskVisual(diskType)
		typeVisuals[diskType] = visual
	}

	config := Config{
		DiskRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:    typeVisuals,
			StableIDToSlot: stableIDToSlot,
			TotalSlots:     len(disks) + 2, // Found disks + 2 extra slots
			SlotWidth:      12,
			SlotsPerRow:    6,
			ShowTemp:       true,
			ShowSmart:      true,
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

	printSuccess("Configuration created successfully based on detected hardware")
	printInfo(fmt.Sprintf("Total disk slots: %d", len(disks)))
	printInfo(fmt.Sprintf("Disk types found: %d", len(typeGroups)))
	if globalHasHealth {
		printInfo("SMART checking enabled based on detected capabilities")
	} else {
		printInfo("SMART checking disabled (no SMART data available)")
	}

	return nil
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

	// Set defaults
	if cfg.SmartTimeout == 0 {
		cfg.SmartTimeout = 10
	}

	return &cfg, nil
}

func checkDiskAgainstRequirements(disk DiskInfo, requirements []DiskRequirement, config *Config) DiskCheckResult {
	result := DiskCheckResult{
		Status:   "ok",
		SizeOK:   true,
		TypeOK:   true,
		HealthOK: true,
		TempOK:   true,
		ModelOK:  true,
	}

	var matchingReqs []DiskRequirement
	for _, req := range requirements {
		if req.RequiredType != "" && req.RequiredType != "any" && disk.Type != req.RequiredType {
			continue
		}
		matchingReqs = append(matchingReqs, req)
	}

	if len(matchingReqs) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range matchingReqs {
		// Check size
		if req.MinSizeGB > 0 && disk.SizeGB < req.MinSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Size too small: %dGB (required %dGB)", disk.SizeGB, req.MinSizeGB))
			result.SizeOK = false
			hasErrors = true
		}

		if req.MaxSizeGB > 0 && disk.SizeGB > req.MaxSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Size too large: %dGB (max %dGB)", disk.SizeGB, req.MaxSizeGB))
			result.SizeOK = false
			hasErrors = true
		}

		// Check health (if enabled)
		if config.CheckSmart && req.RequireHealth {
			if disk.SmartStatus == "N/A" {
				result.Issues = append(result.Issues, "SMART status unavailable")
				result.HealthWarn = true
				hasWarnings = true
			} else if disk.SmartStatus == "FAILED" {
				result.Issues = append(result.Issues, "SMART health check failed")
				result.HealthOK = false
				hasErrors = true
			}
		}

		// Check temperature (if enabled)
		if config.CheckTemp && req.MaxTempC > 0 && disk.Temperature > 0 {
			if disk.Temperature > req.MaxTempC {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Temperature too high: %d°C (max %d°C)", disk.Temperature, req.MaxTempC))
				result.TempOK = false
				hasErrors = true
			}
		}

		// Check model
		if req.RequiredModel != "" && !strings.Contains(disk.Model, req.RequiredModel) {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Model mismatch: %s (required %s)", disk.Model, req.RequiredModel))
			result.ModelOK = false
			hasErrors = true
		}

		// Check removable
		if !req.AllowRemovable && disk.IsRemovable {
			result.Issues = append(result.Issues, "Removable drive not allowed")
			hasWarnings = true
		}

		// Check power-on hours
		if req.MaxPowerOnHrs > 0 && disk.PowerOnHrs > req.MaxPowerOnHrs {
			result.Issues = append(result.Issues,
				fmt.Sprintf("High power-on hours: %d (max %d)", disk.PowerOnHrs, req.MaxPowerOnHrs))
			hasWarnings = true
		}
	}

	if hasErrors {
		result.Status = "error"
	} else if hasWarnings {
		result.Status = "warning"
	}

	return result
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

func visualizeSlots(disks []DiskInfo, config *Config) error {
	printInfo("Disk Slots Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(disks) + 2
	}

	// Create slot data array
	slotData := make([]DiskInfo, maxSlots+1) // +1 because slots start from 1
	slotResults := make([]DiskCheckResult, maxSlots+1)

	// Fill slots based on stable ID to slot mapping
	foundStableIDs := make(map[string]bool)
	for _, disk := range disks {
		foundStableIDs[disk.StableID] = true
		if logicalSlot, exists := config.Visualization.StableIDToSlot[disk.StableID]; exists {
			if logicalSlot > 0 && logicalSlot <= maxSlots {
				slotData[logicalSlot] = disk
				slotResults[logicalSlot] = checkDiskAgainstRequirements(disk, config.DiskRequirements, config)
			}
		}
	}

	// Check for missing devices
	hasErrors := false
	hasWarnings := false
	missingDevices := []string{}

	for stableID, expectedSlot := range config.Visualization.StableIDToSlot {
		if expectedSlot > 0 && expectedSlot <= maxSlots {
			if !foundStableIDs[stableID] {
				slotResults[expectedSlot] = DiskCheckResult{Status: "missing"}
				missingDevices = append(missingDevices, fmt.Sprintf("%s (slot %d)", stableID, expectedSlot))
				hasErrors = true

				slotData[expectedSlot] = DiskInfo{
					StableID: stableID,
					Device:   "MISSING",
				}
			}
		}
	}

	// Count status types
	for i := 1; i <= maxSlots; i++ {
		status := slotResults[i].Status
		if status == "error" || status == "missing" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Print legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Present & OK  ", ColorGreen, "████", ColorReset)
	fmt.Printf("  %s%s%s Disk with Issues  ", ColorYellow, "████", ColorReset)
	fmt.Printf("  %s%s%s Missing Disk  ", ColorRed, "░░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░░", ColorReset)
	fmt.Println()

	// Report missing devices
	if len(missingDevices) > 0 {
		printError("Missing devices:")
		for _, device := range missingDevices {
			printError(fmt.Sprintf("  - %s", device))
		}
		fmt.Println()
	}

	// Report detailed issues
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		if len(result.Issues) > 0 {
			diskID := slotData[i].StableID
			if diskID == "" {
				diskID = slotData[i].Device
			}

			if result.Status == "error" {
				printError(fmt.Sprintf("Slot %d (%s) issues:", i, diskID))
			} else if result.Status == "warning" {
				printWarning(fmt.Sprintf("Slot %d (%s) warnings:", i, diskID))
			}

			for _, issue := range result.Issues {
				if result.Status == "error" {
					printError(fmt.Sprintf("  - %s", issue))
				} else if result.Status == "warning" {
					printWarning(fmt.Sprintf("  - %s", issue))
				}
			}
		}
	}

	if hasErrors || hasWarnings {
		fmt.Println()
	}

	// Build visualization
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
			slotIdx := rowStart + i
			visual := getDiskVisual(slotData[slotIdx], &config.Visualization)
			result := slotResults[slotIdx]

			symbolText := centerText(visual.Symbol, width)

			switch result.Status {
			case "ok":
				fmt.Print(ColorGreen + symbolText + ColorReset)
			case "warning", "error":
				fmt.Print(ColorYellow + symbolText + ColorReset)
			case "missing":
				fmt.Print(ColorRed + centerText("░░░░", width) + ColorReset)
			default:
				fmt.Print(symbolText)
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Type row
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			result := slotResults[slotIdx]

			if slotData[slotIdx].StableID != "" || slotData[slotIdx].Device != "" {
				visual := getDiskVisual(slotData[slotIdx], &config.Visualization)
				nameText := centerText(visual.ShortName, width)

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
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Size row
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			result := slotResults[slotIdx]

			if slotData[slotIdx].StableID != "" || slotData[slotIdx].Device != "" {
				var sizeInfo string
				if result.Status == "missing" {
					sizeInfo = "?"
				} else {
					sizeInfo = formatSize(slotData[slotIdx].SizeGB)
				}

				sizeText := centerText(sizeInfo, width)

				if result.Status == "missing" {
					fmt.Print(ColorRed + sizeText + ColorReset)
				} else if !result.SizeOK {
					fmt.Print(ColorRed + sizeText + ColorReset)
				} else if result.Status == "warning" || result.Status == "error" {
					fmt.Print(ColorYellow + sizeText + ColorReset)
				} else if result.Status == "ok" {
					fmt.Print(ColorGreen + sizeText + ColorReset)
				} else {
					fmt.Print(sizeText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Temperature row (if enabled)
		if config.Visualization.ShowTemp {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := rowStart + i
				result := slotResults[slotIdx]

				if slotData[slotIdx].StableID != "" || slotData[slotIdx].Device != "" {
					var tempInfo string
					if result.Status == "missing" {
						tempInfo = "?"
					} else {
						tempInfo = formatTemp(slotData[slotIdx].Temperature)
					}

					tempText := centerText(tempInfo, width)

					if result.Status == "missing" {
						fmt.Print(ColorRed + tempText + ColorReset)
					} else if !result.TempOK {
						fmt.Print(ColorRed + tempText + ColorReset)
					} else if result.Status == "warning" || result.Status == "error" {
						fmt.Print(ColorYellow + tempText + ColorReset)
					} else if result.TempWarn {
						fmt.Print(ColorYellow + tempText + ColorReset)
					} else if result.Status == "ok" {
						fmt.Print(ColorGreen + tempText + ColorReset)
					} else {
						fmt.Print(tempText)
					}
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
				fmt.Print("│")
			}
			fmt.Println()
		}

		// SMART status row (if enabled)
		if config.Visualization.ShowSmart {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := rowStart + i
				result := slotResults[slotIdx]

				if slotData[slotIdx].StableID != "" || slotData[slotIdx].Device != "" {
					var smartInfo string
					if result.Status == "missing" {
						smartInfo = "?"
					} else {
						switch slotData[slotIdx].SmartStatus {
						case "PASSED":
							smartInfo = "OK"
						case "FAILED":
							smartInfo = "FAIL"
						default:
							smartInfo = "N/A"
						}
					}

					smartText := centerText(smartInfo, width)

					if result.Status == "missing" {
						fmt.Print(ColorRed + smartText + ColorReset)
					} else if !result.HealthOK {
						fmt.Print(ColorRed + smartText + ColorReset)
					} else if result.Status == "warning" || result.Status == "error" {
						fmt.Print(ColorYellow + smartText + ColorReset)
					} else if result.HealthWarn {
						fmt.Print(ColorYellow + smartText + ColorReset)
					} else if result.Status == "ok" {
						fmt.Print(ColorGreen + smartText + ColorReset)
					} else {
						fmt.Print(smartText)
					}
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
				fmt.Print("│")
			}
			fmt.Println()
		}

		// Bottom border
		fmt.Print("└")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Slot numbers
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			fmt.Print(centerText(fmt.Sprintf("%d", slotIdx), width+1))
		}
		fmt.Println("(Slot)")

		// Stable IDs
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := rowStart + i
			stableID := ""
			if slotData[slotIdx].StableID != "" {
				// Truncate stable ID for display
				stableID = slotData[slotIdx].StableID
				if len(stableID) > width {
					stableID = stableID[:width-3] + "..."
				}
			} else {
				stableID = "-"
			}
			fmt.Print(centerText(stableID, width+1))
		}
		fmt.Println("(StableID)")

		// Device names
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
		fmt.Println("(Device)")

		fmt.Println()
	}

	// Final status
	if hasErrors {
		printError("Disk configuration validation FAILED!")
		return fmt.Errorf("disk configuration validation failed")
	} else if hasWarnings {
		printWarning("Disk configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All disks present and meet requirements!")
		return nil
	}
}

func getDiskVisual(disk DiskInfo, config *VisualizationConfig) DiskVisual {
	if disk.StableID == "" && disk.Device == "" {
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

func checkDisks(config *Config) error {
	printInfo("Starting disk check...")

	disks, err := getDiskInfo()
	if err != nil {
		return fmt.Errorf("failed to get disk info: %v", err)
	}

	printInfo(fmt.Sprintf("Found disks: %d", len(disks)))

	if len(disks) == 0 {
		printError("No disks found")
		return fmt.Errorf("no disks found")
	}

	// Display found disks
	for i, disk := range disks {
		printInfo(fmt.Sprintf("Disk %d: %s", i+1, disk.StableID))
		printDebug(fmt.Sprintf("  Device: %s", disk.Device))
		printDebug(fmt.Sprintf("  Model: %s", disk.Model))
		printDebug(fmt.Sprintf("  Serial: %s", disk.Serial))
		if disk.WWN != "" {
			printDebug(fmt.Sprintf("  WWN: %s", disk.WWN))
		}
		printDebug(fmt.Sprintf("  Type: %s", disk.Type))
		printDebug(fmt.Sprintf("  Size: %dGB", disk.SizeGB))
		printDebug(fmt.Sprintf("  Interface: %s", disk.Interface))
		printDebug(fmt.Sprintf("  SMART: %s", disk.SmartStatus))
		if disk.Temperature > 0 {
			printDebug(fmt.Sprintf("  Temperature: %d°C", disk.Temperature))
		}
		if len(disk.MountPoints) > 0 {
			printDebug(fmt.Sprintf("  Mounted: %s", strings.Join(disk.MountPoints, ", ")))
		}
	}

	// Check each requirement
	allPassed := true
	for _, req := range config.DiskRequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		matchingDisks := filterDisks(disks, req)

		printInfo(fmt.Sprintf("  Found %d disk(s) matching criteria", len(matchingDisks)))

		if len(matchingDisks) < req.MinCount {
			printError(fmt.Sprintf("  Requirement FAILED: found %d disk(s), required %d", len(matchingDisks), req.MinCount))
			allPassed = false
			continue
		}

		if req.MaxCount > 0 && len(matchingDisks) > req.MaxCount {
			printError(fmt.Sprintf("  Requirement FAILED: found %d disk(s), maximum %d", len(matchingDisks), req.MaxCount))
			allPassed = false
			continue
		}

		// Check each matching disk
		reqPassed := true
		for i, disk := range matchingDisks {
			printInfo(fmt.Sprintf("    Disk %d: %s", i+1, disk.StableID))
			if disk.Device != disk.StableID {
				printDebug(fmt.Sprintf("      Device: %s", disk.Device))
			}

			result := checkDiskAgainstRequirements(disk, []DiskRequirement{req}, config)

			if len(result.Issues) > 0 {
				for _, issue := range result.Issues {
					if result.Status == "error" {
						printError(fmt.Sprintf("      %s", issue))
						reqPassed = false
					} else {
						printWarning(fmt.Sprintf("      %s", issue))
					}
				}
			} else {
				printSuccess(fmt.Sprintf("      All checks passed"))
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
		printSuccess("All disk requirements passed")
	} else {
		printError("Some disk requirements failed")
		return fmt.Errorf("disk requirements not met")
	}

	return nil
}

func filterDisks(disks []DiskInfo, req DiskRequirement) []DiskInfo {
	var matching []DiskInfo

	for _, disk := range disks {
		// Check type filter
		if req.RequiredType != "" && req.RequiredType != "any" && disk.Type != req.RequiredType {
			continue
		}

		// Check specific stable IDs
		if len(req.StableIDs) > 0 {
			found := false
			for _, stableID := range req.StableIDs {
				if disk.StableID == stableID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Check specific slots (legacy support)
		if len(req.RequiredSlots) > 0 {
			found := false
			for _, slot := range req.RequiredSlots {
				if disk.Slot == slot {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		matching = append(matching, disk)
	}

	return matching
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
		printInfo("Scanning for disk drives...")
		disks, err := getDiskInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting disk information: %v", err))
			os.Exit(1)
		}

		if len(disks) == 0 {
			printWarning("No disk drives found")
		} else {
			printSuccess(fmt.Sprintf("Found disk drives: %d", len(disks)))
			for i, disk := range disks {
				fmt.Printf("\nDisk %d:\n", i+1)
				fmt.Printf("  Stable ID: %s\n", disk.StableID)
				fmt.Printf("  Device: %s\n", disk.Device)
				fmt.Printf("  Model: %s\n", disk.Model)
				fmt.Printf("  Serial: %s\n", disk.Serial)
				if disk.WWN != "" {
					fmt.Printf("  WWN: %s\n", disk.WWN)
				}
				fmt.Printf("  Size: %s\n", formatSize(disk.SizeGB))
				fmt.Printf("  Type: %s\n", disk.Type)
				fmt.Printf("  Interface: %s\n", disk.Interface)
				fmt.Printf("  Vendor: %s\n", disk.Vendor)
				fmt.Printf("  Firmware: %s\n", disk.Firmware)
				fmt.Printf("  Slot: %s\n", disk.Slot)
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

	err = checkDisks(config)
	if err != nil {
		printError(fmt.Sprintf("Disk check failed: %v", err))
		os.Exit(1)
	}
}
