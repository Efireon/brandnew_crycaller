package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	serialFile = "SERIAL"
	maxRetries = 3 // Maximum number of retry attempts for critical operations
)

//go:embed rtnicpg
var rtnicpgData embed.FS

//go:embed eeupdate
var eeupdateData embed.FS

var (
	cDir        string // current working directory
	mbSN        string // motherboard serial number (user input)
	ioSN        string // IO serial number (for "Silver" product)
	mac         string // MAC address (user input)
	rtDrv       string // name of removed conflicting driver
	productName string // product name from dmidecode (e.g., "Silver" or "IFMBH610MTPR")
	driverDir   string // directory for finding/saving local drivers

	// MAC flashing method: "rtnicpg" or "eeupdate"
	macFlashingMethod string

	// EFI variable configuration
	guidPrefix string // prefix for UEFI GUID variable
	efiVarGUID string // generated GUID

	// Parameters for efivar
	efiSNName  string // name of UEFI variable for Serial Number
	efiMACName string // name of UEFI variable for MAC address

	// Parameters for logging
	logToFile bool   // flag for saving log to file
	logServer string // server address for sending log (format: user@host:path)

	// Temporary path for extracted rtnicpg files
	tempRtnicpgPath  string
	tempEeupdatePath string
)

// ANSI escape sequences для цветного вывода
const (
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorBlue    = "\033[34m"
	colorCyan    = "\033[36m"
	colorBgRed   = "\033[41m"
	colorBgGreen = "\033[42m"
)

// Section represents a section from dmidecode output
type Section struct {
	Handle     string                 `json:"handle,omitempty"`
	Title      string                 `json:"title,omitempty"`
	Properties map[string]interface{} `json:"properties,omitempty"`
}

// LogData structure for storing process information
type LogData struct {
	Timestamp       string                 `json:"timestamp"`
	ProductName     string                 `json:"product_name"`
	MbSerialNumber  string                 `json:"mb_serial_number"`
	IoSerialNumber  string                 `json:"io_serial_number,omitempty"`
	MacAddress      string                 `json:"mac_address"`
	OriginalSerial  string                 `json:"original_serial"`
	ActionPerformed string                 `json:"action_performed"`
	Success         bool                   `json:"success"`
	SystemInfo      map[string]interface{} `json:"system_info"`
	EfiSNVarName    string                 `json:"efi_sn_var_name,omitempty"`  // для SerialNumber
	EfiMACVarName   string                 `json:"efi_mac_var_name,omitempty"` // для MAC
	EfiVarGUID      string                 `json:"efi_var_guid,omitempty"`
}

func debugPrint(message string) {
	fmt.Println(colorCyan + "DEBUG: " + message + colorReset)
}

// Функция для вывода критических ошибок с яркими плашками
func criticalError(message string) {
	// Создаем рамку для большей заметности
	lineLength := len(message) + 6 // добавляем отступы
	if lineLength < 80 {
		lineLength = 80 // минимальная ширина плашки
	}

	border := strings.Repeat("!", lineLength)
	spaces := strings.Repeat(" ", (lineLength-len(message)-2)/2)

	fmt.Println("")
	fmt.Println(colorBgRed + colorReset)
	fmt.Println(colorBgRed + border + colorReset)
	fmt.Println(colorBgRed + "!!!" + spaces + message + spaces + "!!!" + colorReset)
	fmt.Println(colorBgRed + border + colorReset)
	fmt.Println(colorBgRed + colorReset)
	fmt.Println("")
}

// Функция для вывода информации об успешном завершении
func successMessage(message string) {
	// Создаем рамку для большей заметности
	lineLength := len(message) + 6 // добавляем отступы
	if lineLength < 60 {
		lineLength = 60 // минимальная ширина плашки
	}

	border := strings.Repeat("=", lineLength)
	spaces := strings.Repeat(" ", (lineLength-len(message)-2)/2)

	fmt.Println("")
	fmt.Println(colorBgGreen + colorReset)
	fmt.Println(colorBgGreen + border + colorReset)
	fmt.Println(colorBgGreen + "  " + spaces + message + spaces + "  " + colorReset)
	fmt.Println(colorBgGreen + border + colorReset)
	fmt.Println(colorBgGreen + colorReset)
	fmt.Println("")
}

// Function to prompt for server address for sending logs
func promptForLogServer() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println(colorYellow + "IMPORTANT: Log sending to server is mandatory when -server parameter is used." + colorReset)
	fmt.Print("Enter server address for sending logs (format: user@host:path): ")
	server, _ := reader.ReadString('\n')
	return strings.TrimSpace(server)
}

// Function to ensure log is sent to server (only if -server parameter was specified)
func ensureLogSent(actionPerformed string, success bool, originalSerial string) {
	// Only enforce server logging if -server parameter was set but is empty
	if logServer == "" {
		// If no server was specified, we don't require server logging
		return
	}

	// If we get here, it means logServer was specified but might be empty
	if strings.TrimSpace(logServer) == "" {
		fmt.Println(colorYellow + "Server address for sending logs was not specified." + colorReset)
		logServer = promptForLogServer()

		if logServer != "" {
			// Resend the log with the specified server
			createOperationLog(actionPerformed, success, originalSerial)
		} else {
			criticalError("Log sending is mandatory when -server parameter is used. Please restart the program with a valid server address.")
		}
	}
}

// Модифицируем функцию extractRtnicpg для поддержки eeupdate
func extractEmbeddedFiles() error {
	var err error

	// Extract rtnicpg files
	tempRtnicpgPath, err = extractFSToTemp(rtnicpgData, "rtnicpg-extract-*")
	if err != nil {
		return fmt.Errorf("Failed to extract rtnicpg files: %v", err)
	}
	debugPrint("Extracted rtnicpg to: " + tempRtnicpgPath)

	// Extract eeupdate files
	tempEeupdatePath, err = extractFSToTemp(eeupdateData, "eeupdate-extract-*")
	if err != nil {
		return fmt.Errorf("Failed to extract eeupdate files: %v", err)
	}
	debugPrint("Extracted eeupdate to: " + tempEeupdatePath)

	return nil
}

// Общая функция для извлечения встроенных файлов
func extractFSToTemp(embeddedFS embed.FS, tempPrefix string) (string, error) {
	tempDir, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return "", fmt.Errorf("could not create temporary directory: %v", err)
	}

	// Walk through the embedded filesystem and copy files preserving structure
	err = fs.WalkDir(embeddedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory
		if path == "." {
			return nil
		}

		// Create the target path
		targetPath := filepath.Join(tempDir, path)

		if d.IsDir() {
			// Create directory
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("could not create directory %s: %v", targetPath, err)
			}
		} else {
			// Create directory for the file if it doesn't exist
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("could not create parent directory for %s: %v", targetPath, err)
			}

			// Read file from embedded filesystem
			data, err := embeddedFS.ReadFile(path)
			if err != nil {
				return fmt.Errorf("could not read embedded file %s: %v", path, err)
			}

			// Write file to the temporary directory
			if err := os.WriteFile(targetPath, data, 0644); err != nil {
				return fmt.Errorf("could not write file %s: %v", targetPath, err)
			}

			// If the file is executable, set executable permissions
			if strings.HasSuffix(targetPath, "-x86_64") ||
				strings.HasSuffix(targetPath, "-aarch64") ||
				strings.HasSuffix(targetPath, ".sh") ||
				strings.Contains(targetPath, "eeupdate64e") {
				if err := os.Chmod(targetPath, 0755); err != nil {
					return fmt.Errorf("could not set executable permissions for %s: %v", targetPath, err)
				}
			}
		}

		return nil
	})

	if err != nil {
		os.RemoveAll(tempDir) // Clean up on error
		return "", fmt.Errorf("error extracting embedded files: %v", err)
	}

	return tempDir, nil
}

func main() {
	// Add flags for logging and EFI variables
	logFilePtr := flag.Bool("log", true, "Save log to file")
	logServerPtr := flag.String("server", "", "Server to send log to (format: user@host:path)")
	guidPrefixPtr := flag.String("guid-prefix", "", "Optional 8-hex-digit prefix for the generated GUID")
	efiSNPtr := flag.String("efisn", "SerialNumber", "Name of the UEFI variable for Serial Number (default: SerialNumber)")
	efiMACPtr := flag.String("efimac", "HexMac", "Name of the UEFI variable for MAC Address (default: HexMac)")
	driverDirPtr := flag.String("driver", "", "Directory for finding/saving local drivers")
	flag.Parse()

	logToFile = *logFilePtr
	logServer = *logServerPtr
	guidPrefix = *guidPrefixPtr
	efiSNName = *efiSNPtr
	efiMACName = *efiMACPtr
	driverDir = *driverDirPtr

	// Root privileges are required
	if os.Geteuid() != 0 {
		criticalError("Please run this program with root privileges")
		os.Exit(1)
	}

	var err error
	cDir, err = os.Getwd()
	if err != nil {
		criticalError("Could not get current directory: " + err.Error())
		os.Exit(1)
	}

	// Extract embedded files
	if err := extractEmbeddedFiles(); err != nil {
		criticalError("Failed to extract embedded files: " + err.Error())
		os.Exit(1)
	}

	// Register cleanup of temporary directories on exit
	defer os.RemoveAll(tempRtnicpgPath)
	defer os.RemoveAll(tempEeupdatePath)

	debugPrint("Extracted rtnicpg to: " + tempRtnicpgPath)
	debugPrint("Extracted eeupdate to: " + tempEeupdatePath)

	fmt.Println(colorBlue + "Starting serial number modification..." + colorReset)

	// 1. Read serial numbers and MAC from the user
	if err := getSerialAndMac(); err != nil {
		criticalError("Failed to get serial and MAC: " + err.Error())
		os.Exit(1)
	}

	debugPrint("User provided MB Serial: " + mbSN)
	if ioSN != "" {
		debugPrint("User provided IO Serial: " + ioSN)
	}
	debugPrint("User provided MAC: " + mac)

	// 2. Get system serial numbers via dmidecode
	baseSerial, err := getSystemSerial("baseboard")
	if err != nil {
		log.Printf(colorYellow+"[WARNING] Could not get baseboard serial: %v"+colorReset, err)
	} else {
		debugPrint("System Baseboard Serial: " + baseSerial)
	}

	sysSerial, err := getSystemSerial("system")
	if err != nil {
		log.Printf(colorYellow+"[WARNING] Could not get system serial: %v"+colorReset, err)
	} else {
		debugPrint("System Serial: " + sysSerial)
	}

	// Determine if serial number reflashing is required
	needSerialFlash := false

	// Тщательно проверяем логику сравнения серийных номеров
	// При этом используем ТОЛЬКО mbSN для сравнения, IO Serial не проверяется
	if mbSN != baseSerial {
		needSerialFlash = true
		debugPrint("Serial flashing is required: current SN does not match requested")
		debugPrint(fmt.Sprintf("Current baseboard SN: %s, requested mbSN: %s", baseSerial, mbSN))
	} else {
		debugPrint("Serial numbers match, no flashing required")
	}

	// ioSN не проверяется, он используется только для логов
	if productName == "Silver" {
		debugPrint(fmt.Sprintf("IO SN (%s) is only used for logging purposes, not for comparison", ioSN))
	} else if productName == "IFMBH610MTPR" {
		// Для IFMBH610MTPR продукта сравниваем только mbSN с baseSerial
		if mbSN != baseSerial {
			needSerialFlash = true
			debugPrint("Serial flashing is required: current SN does not match requested")
			debugPrint(fmt.Sprintf("Current baseboard SN: %s, requested: %s", baseSerial, mbSN))
		} else {
			debugPrint("Serial numbers match, no flashing required")
		}
	} else if productName == "IFMBB760M" {
		// Для IFMBH610MTPR продукта сравниваем только mbSN с baseSerial
		if mbSN != baseSerial {
			needSerialFlash = true
			debugPrint("Serial flashing is required: current SN does not match requested")
			debugPrint(fmt.Sprintf("Current baseboard SN: %s, requested: %s", baseSerial, mbSN))
		} else {
			debugPrint("Serial numbers match, no flashing required")
		}
	}

	// Determine if entered MAC matches what is already present
	targetMAC := strings.ToLower(mac)
	macAlreadySet := false
	if ifaces, err := getInterfacesWithMAC(targetMAC); err == nil && len(ifaces) > 0 {
		macAlreadySet = true
		debugPrint(fmt.Sprintf("MAC %s is already present on interfaces: %s", targetMAC, strings.Join(ifaces, ", ")))
	} else {
		debugPrint(fmt.Sprintf("MAC %s not found in system, flashing is required", targetMAC))
	}

	// Variable to record performed actions
	actionPerformed := ""
	success := true

	reader := bufio.NewReader(os.Stdin)

	// Handle different scenarios based on what needs updating
	if !needSerialFlash && macAlreadySet {
		// CASE 1: Both serial numbers and MAC already match - no changes needed
		actionPerformed = "No changes required"
		successMessage("No reflash required – system already has the correct serial number and MAC address")

		// Create log before completion
		createOperationLog(actionPerformed, success, baseSerial)

		// Ensure log is sent to server
		ensureLogSent(actionPerformed, success, baseSerial)

		fmt.Print("Poweroff system now? (Y/n): ")
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)
		if !strings.EqualFold(choice, "n") {
			fmt.Println("Powering off system...")
			_ = runCommandNoOutput("poweroff")
		} else {
			fmt.Println("Exiting without powering off. Please shutdown manually.")
		}
	} else if !needSerialFlash && !macAlreadySet {
		// CASE 2: Serial numbers match but MAC needs updating
		actionPerformed = "MAC address update only"
		fmt.Println(colorYellow + "Serial numbers match. Only MAC flash is required." + colorReset)

		// Clear any existing MAC EFI variables
		if err := clearAllMacEfiVariables(); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to clear all MAC EFI variables: %v\n"+colorReset, err)
		} else {
			debugPrint("Cleared all MAC EFI variables")
		}

		// Пытаемся обновить MAC через драйвер с повторными попытками
		if err := writeMAcWithRetries(mac); err != nil {
			success = false
			criticalError("MAC address could not be written after multiple attempts. It is recommended to power off the system and diagnose the hardware manually.")
		} else {
			// Generate GUID for EFI variables if not already generated
			if efiVarGUID == "" {
				efiVarGUID, err = randomGUIDWithPrefix(guidPrefix)
				if err != nil {
					fmt.Printf(colorYellow+"[WARNING] Failed to generate GUID: %v\n"+colorReset, err)
				} else {
					debugPrint("Generated EFI variable GUID: " + efiVarGUID)
				}
			}

			// MAC будет записан в EFI-переменную в функции writeMAcWithRetries
		}

		// Создаём лог
		createOperationLog(actionPerformed, success, baseSerial)

		// Ensure log is sent to server
		ensureLogSent(actionPerformed, success, baseSerial)

		if success {
			successMessage("MAC address updated successfully")
		}

		fmt.Print("Poweroff system now? (Y/n): ")
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)
		if !strings.EqualFold(choice, "n") {
			fmt.Println("Powering off system...")
			_ = runCommandNoOutput("poweroff")
		} else {
			fmt.Println("Exiting without powering off. Please shutdown manually.")
		}
	} else if needSerialFlash {
		// CASE 3: Serial numbers need updating (MAC may or may not need updating)
		// Определяем, какие действия будут выполняться:
		if !macAlreadySet {
			actionPerformed = "Serial number and MAC address update"
		} else {
			actionPerformed = "Serial number update only"
		}

		if err := clearEfiVariables(efiSNName); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to clear extra EFI variables for %s: %v\n"+colorReset, efiSNName, err)
		} else {
			debugPrint("Cleared extra EFI variables for " + efiSNName)
		}

		// Очищаем все MAC-переменные ДО обновлений
		if err := clearAllMacEfiVariables(); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to clear all MAC EFI variables: %v\n"+colorReset, err)
		} else {
			debugPrint("Cleared all MAC EFI variables")
		}

		// First, flash MAC if it's not already set
		if !macAlreadySet {
			if err := writeMAcWithRetries(mac); err != nil {
				success = false
				criticalError("MAC address could not be written after multiple attempts. It is recommended to power off the system and diagnose the hardware manually.")

				// Create log before exiting
				createOperationLog("MAC address update failed", false, baseSerial)

				// Ensure log is sent to server
				ensureLogSent("MAC address update failed", false, baseSerial)

				fmt.Print("Poweroff system now? (Y/n): ")
				choice, _ := reader.ReadString('\n')
				choice = strings.TrimSpace(choice)
				if !strings.EqualFold(choice, "n") {
					fmt.Println("Powering off system...")
					_ = runCommandNoOutput("poweroff")
				} else {
					fmt.Println("Exiting without powering off. Please shutdown manually.")
				}
				os.Exit(1)
			}
		} else {
			fmt.Println(colorGreen + "[INFO] MAC address already set correctly, skipping MAC update." + colorReset)
		}

		// Clear existing EFI variables for both Serial Number and MAC
		if err := clearEfiVariables(efiSNName); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to clear extra EFI variables for %s: %v\n"+colorReset, efiSNName, err)
		} else {
			debugPrint("Cleared extra EFI variables for " + efiSNName)
		}

		// Now update serial number
		// Generate GUID for EFI variable
		efiVarGUID, err = randomGUIDWithPrefix(guidPrefix)
		if err != nil {
			success = false
			criticalError("Failed to generate GUID: " + err.Error())
			os.Exit(1)
		}
		debugPrint("Generated EFI variable GUID: " + efiVarGUID)

		// Write serial number to EFI variable with retries
		var serialWriteSuccess bool = false

		// В EFI-переменную и в файл SERIAL всегда записываем только mbSN
		// ioSN используется только для логов

		for retry := 0; retry < maxRetries; retry++ {
			if err := writeSerialToEfiVar(mbSN); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Attempt %d: Failed to write serial to EFI variable: %v"+colorReset+"\n", retry+1, err)
				if retry == maxRetries-1 {
					criticalError("Failed to write serial to EFI variable after multiple attempts. It is recommended to power off the system and diagnose the hardware manually.")

					// Create log before exiting
					createOperationLog("Serial number update failed", false, baseSerial)

					// Ensure log is sent to server
					ensureLogSent("Serial number update failed", false, baseSerial)

					fmt.Print("Poweroff system now? (Y/n): ")
					choice, _ := reader.ReadString('\n')
					choice = strings.TrimSpace(choice)
					if !strings.EqualFold(choice, "n") {
						fmt.Println("Powering off system...")
						_ = runCommandNoOutput("poweroff")
					} else {
						fmt.Println("Exiting without powering off. Please shutdown manually.")
					}
					os.Exit(1)
				}
				time.Sleep(500 * time.Millisecond) // Small delay between retries
				continue
			}

			// Successfully written, no need to check reading as it causes errors
			debugPrint("Successfully wrote serial number to EFI variable")
			serialWriteSuccess = true
			break
		}

		if !serialWriteSuccess {
			success = false
			criticalError("Failed to write serial to EFI variable after multiple attempts")
			os.Exit(1)
		}

		// If MAC was successfully set, record MAC addresses for EFI variables
		var allMACs []string
		if !macAlreadySet || macAlreadySet {
			allMACs = append(allMACs, targetMAC)

			// Try to find other sequential MAC addresses if they exist
			currentMAC := targetMAC
			for i := 1; i < 8; i++ { // Check up to 8 possible sequential MACs
				var err error
				currentMAC, err = incrementMAC(currentMAC)
				if err != nil {
					break
				}

				if ifaces, err := getInterfacesWithMAC(currentMAC); err == nil && len(ifaces) > 0 {
					allMACs = append(allMACs, currentMAC)
					debugPrint(fmt.Sprintf("Found interface with MAC %s", currentMAC))
				} else {
					break
				}
			}

			// Write all found MACs to EFI variables
			if err := writeAllMACsToEfiVars(allMACs); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to write all MAC addresses to EFI variables: %v\n"+colorReset, err)
			} else {
				debugPrint(fmt.Sprintf("Successfully wrote %d MAC addresses to EFI variables", len(allMACs)))
			}
		}

		// No need to write to file anymore as we're using EFI variables now
		debugPrint("Using EFI variables to store serial number, file method is deprecated")

		// Call bootctl function to set up one-time boot entry and reflash EFI
		if err := bootctl(); err != nil {
			success = false
			criticalError("Bootctl error: " + err.Error())
			os.Exit(1)
		}

		// Create log before reboot
		createOperationLog(actionPerformed, success, baseSerial)

		// Ensure log is sent to server
		ensureLogSent(actionPerformed, success, baseSerial)

		if success {
			successMessage("Serial number has been set successfully")
		}

		// Request system reboot
		fmt.Print("Serial number has been set. Reboot now? (Y/n): ")
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)
		if strings.EqualFold(choice, "n") {
			fmt.Println("Please reboot manually to apply changes.")
		} else {
			fmt.Println("Rebooting system...")
			_ = runCommandNoOutput("reboot")
		}
	} else {
		// This case should never happen logically, but just in case
		fmt.Println(colorGreen + "No changes were required. Exiting..." + colorReset)
		createOperationLog("No changes required", true, baseSerial)

		// Ensure log is sent to server
		ensureLogSent("No changes required", true, baseSerial)
	}
}

// randomGUIDWithPrefix generates a GUID in the format 8-4-4-4-12 (hex), where
// the first 8 hex characters can be specified by prefix. The remaining blocks are generated randomly.
func randomGUIDWithPrefix(prefix string) (string, error) {
	// Prefix must consist of exactly 8 hex characters.
	// If empty, generate random 8.
	if prefix == "" {
		p, err := randomHex(4) // Generate 4 bytes => 8 hex characters
		if err != nil {
			return "", err
		}
		prefix = p
	} else {
		if len(prefix) != 8 {
			return "", errors.New("guid-prefix must have exactly 8 hex characters")
		}
		if _, err := hex.DecodeString(prefix); err != nil {
			return "", fmt.Errorf("invalid guid-prefix: %v", err)
		}
	}

	// Generate another 4-4-4-12 => 24 hex characters.
	suffixBytes, err := randomBytes(12)
	if err != nil {
		return "", fmt.Errorf("random generation error: %v", err)
	}

	suffixHex := hex.EncodeToString(suffixBytes) // 24 hex characters

	// Split into blocks: 4-4-4-12
	block1 := suffixHex[0:4]
	block2 := suffixHex[4:8]
	block3 := suffixHex[8:12]
	block4 := suffixHex[12:24]

	// Form GUID, returning string and nil
	guid := fmt.Sprintf("%s-%s-%s-%s-%s",
		strings.ToLower(prefix),
		block1,
		block2,
		block3,
		block4,
	)

	return guid, nil
}

// randomBytes reads n random bytes from crypto/rand
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// randomHex generates n random bytes and returns them in hex form
func randomHex(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeSerialToEfiVar writes the serial number to an EFI variable
func writeSerialToEfiVar(serialNumber string) error {
	// Create a temporary file to pass data to efivar
	tmpFile, err := os.CreateTemp("", "serial-*.bin")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write the serial number to the temporary file
	if _, err := tmpFile.Write([]byte(serialNumber)); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write to temporary file: %v", err)
	}
	tmpFile.Close()

	// Full variable name
	varName := fmt.Sprintf("%s-%s", efiVarGUID, efiSNName)
	debugPrint("Writing to EFI variable: " + varName)

	// Run efivar to write the variable
	cmd := exec.Command(
		"efivar",
		"--write",
		"--name="+varName,
		"--attributes=7", // Non-volatile + BootService access + RuntimeService access = 7
		"--datafile="+tmpFile.Name(),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to write EFI variable: %v (output: %s)", err, string(out))
	}

	fmt.Printf(colorGreen+"[INFO] Successfully wrote serial number to EFI variable '%s'\n"+colorReset, varName)
	return nil
}

// writeMACToEfiVar writes the MAC address to an EFI variable
func writeMACToEfiVar(macAddress string, index int) error {
	// Определяем имя переменной в зависимости от индекса
	varName := efiMACName
	if index > 0 {
		varName = fmt.Sprintf("%s%d", efiMACName, index)
	}

	debugPrint(fmt.Sprintf("Writing MAC %s to EFI variable %s", macAddress, varName))

	// Create a temporary file to pass data to efivar
	tmpFile, err := os.CreateTemp("", "mac-*.bin")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write the MAC address to the temporary file
	if _, err := tmpFile.Write([]byte(macAddress)); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write to temporary file: %v", err)
	}
	tmpFile.Close()

	// Full variable name
	fullVarName := fmt.Sprintf("%s-%s", efiVarGUID, varName)
	debugPrint("Writing to EFI variable: " + fullVarName)

	// Run efivar to write the variable
	cmd := exec.Command(
		"efivar",
		"--write",
		"--name="+fullVarName,
		"--attributes=7", // Non-volatile + BootService access + RuntimeService access = 7
		"--datafile="+tmpFile.Name(),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to write EFI variable: %v (output: %s)", err, string(out))
	}

	fmt.Printf(colorGreen+"[INFO] Successfully wrote MAC address to EFI variable '%s'\n"+colorReset, fullVarName)
	return nil
}

func writeMACToEfiVarLegacy(macAddress string) error {
	return writeMACToEfiVar(macAddress, 0)
}

// This function is no longer used as we're now using EFI variables
// Left here for reference but could be removed in future versions
/*
func writeSerialToFile(serial string) error {
	return nil
}
*/
func clearAllMacEfiVariables() error {
	// Очищаем основную переменную
	if err := clearEfiVariables(efiMACName); err != nil {
		fmt.Printf(colorYellow+"[WARNING] Failed to clear EFI variables for %s: %v\n"+colorReset, efiMACName, err)
	} else {
		debugPrint("Cleared EFI variables for " + efiMACName)
	}

	// Очищаем нумерованные переменные (до 8)
	for i := 1; i <= 8; i++ {
		varName := fmt.Sprintf("%s%d", efiMACName, i)
		if err := clearEfiVariables(varName); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to clear EFI variables for %s: %v\n"+colorReset, varName, err)
		} else {
			debugPrint("Cleared EFI variables for " + varName)
		}
	}

	return nil
}

// clearEfiVariables removes all EFI variable files in /sys/firmware/efi/efivars/
// whose names start with varName + "-" (e.g. "SerialNumber-*")
func clearEfiVariables(varName string) error {
	// Path to the EFI variables directory
	efiVarsDir := "/sys/firmware/efi/efivars"

	// Read all entries in the directory
	entries, err := os.ReadDir(efiVarsDir)
	if err != nil {
		return fmt.Errorf("failed to read EFI variables directory %s: %v", efiVarsDir, err)
	}

	// The target prefix is varName followed by a dash
	targetPrefix := varName + "-"
	foundVariables := false

	fmt.Printf("[DEBUG] Looking for EFI variables starting with '%s'\n", targetPrefix)

	for _, entry := range entries {
		fileName := entry.Name()
		// Check if the variable file name starts with the target prefix
		if strings.HasPrefix(fileName, targetPrefix) {
			foundVariables = true
			fmt.Printf("[DEBUG] Found matching variable: %s\n", fileName)

			// Build the full file path in /sys/firmware/efi/efivars/
			filePath := filepath.Join(efiVarsDir, fileName)

			// First try to remove the immutable attribute using chattr
			chattrCmd := exec.Command("chattr", "-i", filePath)
			chattrOut, chattrErr := chattrCmd.CombinedOutput()
			if chattrErr != nil {
				fmt.Printf("[WARNING] Failed to remove immutable attribute from %s: %v\nOutput: %s\n",
					filePath, chattrErr, string(chattrOut))
				// Continue anyway - the file might not have the immutable attribute
			} else {
				fmt.Printf("[DEBUG] Removed immutable attribute from %s\n", filePath)
			}

			// Now attempt to delete the file
			if err := os.Remove(filePath); err != nil {
				fmt.Printf("[WARNING] Failed to remove EFI variable file %s: %v\n", filePath, err)

				// If direct deletion fails, try using rm command which might have more permissions
				rmCmd := exec.Command("rm", "-f", filePath)
				rmOut, rmErr := rmCmd.CombinedOutput()
				if rmErr != nil {
					fmt.Printf("[WARNING] Failed to remove EFI variable using rm command: %s: %v\nOutput: %s\n",
						filePath, rmErr, string(rmOut))
				} else {
					fmt.Printf("[INFO] Successfully removed EFI variable file: %s using rm command\n", filePath)
				}
			} else {
				fmt.Printf("[INFO] Successfully removed EFI variable file: %s\n", filePath)
			}
		}
	}

	if !foundVariables {
		fmt.Printf("[INFO] No existing EFI variables found for '%s'\n", varName)
	}

	return nil
}

// pingServer checks server availability before sending logs
// Attempts to ping the server for 15 seconds, waiting for a response
func pingServer(serverAddr string) bool {
	// Extract hostname from the string in format user@host:path
	parts := strings.SplitN(serverAddr, "@", 2)
	if len(parts) != 2 {
		fmt.Printf(colorYellow+"[WARNING] Invalid server address format: %s"+colorReset+"\n", serverAddr)
		return false
	}

	// Extract host from the second part (may contain port or path)
	hostParts := strings.SplitN(parts[1], ":", 2)
	host := hostParts[0]

	fmt.Printf("[INFO] Checking server availability %s (waiting up to 15 seconds)...\n", host)

	// Set total timeout to 15 seconds
	totalTimeout := 15 * time.Second
	startTime := time.Now()
	endTime := startTime.Add(totalTimeout)

	// Interval between retry attempts
	pingInterval := 1 * time.Second

	// Try to ping the server until the total timeout expires
	attempt := 1
	for time.Now().Before(endTime) {
		// Create context with a short timeout for each ping attempt
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		// Use different parameters for ping depending on OS
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "ping", "-n", "1", "-w", "2000", host)
		} else {
			cmd = exec.CommandContext(ctx, "ping", "-c", "1", "-W", "2", host)
		}

		// Hide ping command output
		var dummy bytes.Buffer
		cmd.Stdout = &dummy
		cmd.Stderr = &dummy

		// Execute ping command
		fmt.Printf("[INFO] Ping attempt %d to server %s...\n", attempt, host)
		err := cmd.Run()
		cancel() // Always cancel the context after command execution

		if err == nil {
			// Ping successful
			elapsed := time.Since(startTime).Round(time.Millisecond)
			fmt.Printf(colorGreen+"[INFO] Server %s is available (attempt %d, total time: %v)"+colorReset+"\n",
				host, attempt, elapsed)
			return true
		}

		// Ping failed, check if there's still time
		remainingTime := endTime.Sub(time.Now())
		if remainingTime < pingInterval {
			break // Time left is less than needed for next attempt
		}

		// Wait before next attempt
		time.Sleep(pingInterval)
		attempt++
	}

	// If we exit the loop, all attempts were unsuccessful
	fmt.Printf(colorYellow+"[WARNING] Server %s is unavailable after %d attempts over %v"+colorReset+"\n",
		host, attempt, totalTimeout)
	return false
}

// createOperationLog создает и сохраняет журнал операций
func createOperationLog(action string, success bool, originalSerial string) {
	fmt.Println(colorBlue + "Creating operation log..." + colorReset)

	// Get full dmidecode output
	dmidecodeOutput, err := runCommand("dmidecode")
	if err != nil {
		fmt.Printf(colorYellow+"[WARNING] Could not get dmidecode output for log: %v"+colorReset, err)
		dmidecodeOutput = "Error getting dmidecode output"
	}

	// Parse dmidecode output
	sections, err := parseDmidecodeOutput(dmidecodeOutput)
	if err != nil {
		fmt.Printf(colorYellow+"[WARNING] Could not parse dmidecode output: %v"+colorReset, err)
	}

	// Convert sections to a map for JSON
	systemInfo := make(map[string]interface{})
	for _, sec := range sections {
		key := sec.Title
		if key == "" {
			key = sec.Handle // Use handle if title is empty
		}

		sectionData := make(map[string]interface{})
		if sec.Handle != "" {
			sectionData["handle"] = sec.Handle
		}

		// Only include properties if they are not empty
		if len(sec.Properties) > 0 {
			sectionData["properties"] = sec.Properties
		}

		// If such a key already exists, convert value to an array or add to existing array
		if existing, exists := systemInfo[key]; exists {
			switch v := existing.(type) {
			case map[string]interface{}:
				// If we already have one section with this title, create an array of two elements
				systemInfo[key] = []interface{}{v, sectionData}
			case []interface{}:
				// If we already have an array of sections with this title, add to it
				systemInfo[key] = append(v, sectionData)
			default:
				// For other cases (e.g., if for some reason the value is not a map or slice)
				systemInfo[key] = []interface{}{existing, sectionData}
			}
		} else {
			// If the key doesn't exist yet, just add the section as is
			systemInfo[key] = sectionData
		}
	}

	// Create log data structure
	timestamp := time.Now().Format("2006-01-02T15:04:05")
	logData := LogData{
		Timestamp:       timestamp,
		ProductName:     productName,
		MbSerialNumber:  mbSN,
		IoSerialNumber:  ioSN,
		MacAddress:      mac,
		OriginalSerial:  originalSerial,
		ActionPerformed: action,
		Success:         success,
		SystemInfo:      systemInfo,
		EfiSNVarName:    efiSNName,
		EfiMACVarName:   efiMACName,
		EfiVarGUID:      efiVarGUID,
	}

	// Convert to JSON
	jsonData, err := json.MarshalIndent(logData, "", "  ")
	if err != nil {
		fmt.Printf(colorYellow+"[WARNING] Could not create JSON log: %v"+colorReset, err)
		return
	}

	// Generate filename for the log
	timeFormat := time.Now().Format("060102150405") // YYMMDDHHMMSS
	filename := fmt.Sprintf("%s_%s-%s.json", productName, mbSN, timeFormat)

	// Save log to file if flag is set
	var logSaved bool = false
	var logRetries int = 0
	maxLogRetries := 15

	for !logSaved && logRetries < maxLogRetries {
		logRetries++

		if logToFile {
			logDir := filepath.Join(cDir, "logs")
			// Create log directory if it doesn't exist
			if _, err := os.Stat(logDir); os.IsNotExist(err) {
				if err := os.Mkdir(logDir, 0755); err != nil {
					fmt.Printf(colorYellow+"[WARNING] Could not create log directory: %v. Retry attempt %d/%d"+colorReset, err, logRetries, maxLogRetries)
					logDir = cDir
				}
			}

			logPath := filepath.Join(logDir, filename)
			if err := os.WriteFile(logPath, jsonData, 0644); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Could not write log file: %v. Retry attempt %d/%d\n"+colorReset, err, logRetries, maxLogRetries)
				time.Sleep(500 * time.Millisecond) // Small delay between retries
			} else {
				fmt.Printf(colorGreen+"[INFO] Log saved to: %s\n"+colorReset, logPath)
				logSaved = true
			}
		} else {
			logSaved = true // Skip if logging to file is disabled
		}
	}

	if !logSaved && logToFile {
		// Final attempt to save locally in the current directory if all retries failed
		emergencyLogPath := filepath.Join(cDir, filename)
		if err := os.WriteFile(emergencyLogPath, jsonData, 0644); err != nil {
			criticalError("Failed to save log after multiple attempts. Final error: " + err.Error())
		} else {
			fmt.Printf(colorYellow+"[ATTENTION] Log could not be saved to logs directory after %d attempts. Emergency save to current directory: %s\n"+colorReset, maxLogRetries, emergencyLogPath)
			logSaved = true
		}
	}

	// Send log to server if specified
	var serverLogSent bool = false
	var serverRetries int = 0

	if logServer != "" && strings.TrimSpace(logServer) != "" {
		// Perform preliminary ping of the server before sending logs
		serverPingSuccess := pingServer(logServer)

		if !serverPingSuccess {
			fmt.Printf(colorYellow+"[WARNING] Server %s is unreachable via ping. Log transfer may take longer than expected.\n"+colorReset, logServer)
			// Continue with log transfer even if ping fails,
			// as SCP may still work even if ICMP is blocked
		} else {
			fmt.Printf(colorGreen+"[INFO] Server %s is available, proceeding with log transfer.\n"+colorReset, logServer)
		}

		for !serverLogSent && serverRetries < maxLogRetries {
			serverRetries++

			// Create temporary file
			tempFile, err := os.CreateTemp("", "serial-log-*.json")
			if err != nil {
				fmt.Printf(colorYellow+"[WARNING] Could not create temporary file for log: %v. Retry attempt %d/%d"+colorReset, err, serverRetries, maxLogRetries)
				time.Sleep(500 * time.Millisecond)
				continue
			}

			// Write JSON to file
			if _, err := tempFile.Write(jsonData); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Could not write to temporary file: %v. Retry attempt %d/%d"+colorReset, err, serverRetries, maxLogRetries)
				tempFile.Close()
				os.Remove(tempFile.Name())
				time.Sleep(500 * time.Millisecond)
				continue
			}
			tempFile.Close()

			// Parse server string to host and path
			var host, remotePath string
			parts := strings.SplitN(logServer, ":", 2)

			host = parts[0]
			if len(parts) > 1 {
				remotePath = parts[1]
			}

			// Create remote directory before sending file
			if remotePath != "" {
				// Remove trailing slash if present
				remotePath = strings.TrimSuffix(remotePath, "/")

				// Create directory on remote server with 3-second timeout
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()

				mkdirCmd := exec.CommandContext(ctx, "ssh",
					"-o", "StrictHostKeyChecking=no",
					"-o", "UserKnownHostsFile=/dev/null",
					host, "mkdir", "-p", remotePath)
				_, err := mkdirCmd.CombinedOutput()
				if err != nil {
					fmt.Printf(colorYellow+"[WARNING] Could not create remote directory: %v. Retry attempt %d/%d"+colorReset, err, serverRetries, maxLogRetries)
				}
			}

			// Build correct path for SCP
			var destination string
			if remotePath != "" {
				destination = fmt.Sprintf("%s:%s/%s", host, remotePath, filename)
			} else {
				destination = fmt.Sprintf("%s:%s", host, filename)
			}

			// Send file to server using SCP with 3-second timeout
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "scp",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				tempFile.Name(), destination)
			output, err := cmd.CombinedOutput()

			// Clean up temporary file regardless of the result
			os.Remove(tempFile.Name())

			if err != nil {
				fmt.Printf(colorYellow+"[WARNING] Could not send log to server: %v\nOutput: %s\nRetry attempt %d/%d\n"+colorReset, err, output, serverRetries, maxLogRetries)
				time.Sleep(1 * time.Second) // Longer delay for network operations
			} else {
				fmt.Printf(colorGreen+"[INFO] Log sent to server: %s\n"+colorReset, destination)
				serverLogSent = true
				break
			}
		}

		if !serverLogSent {
			criticalError("Failed to send log to server " + logServer + " after multiple attempts")
		}
	}
}

// parseDmidecodeOutput parses dmidecode output and splits it into sections
func parseDmidecodeOutput(output string) ([]Section, error) {
	var sections []Section
	var currentSection *Section
	expectingTitle := false
	var currentPropKey string

	// Collect header lines (until the first line starting with "Handle")
	headerLines := []string{}
	inHeader := true

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if inHeader {
			if strings.HasPrefix(line, "Handle") {
				// If the header is not empty, add it as a section with "Header" title
				if len(headerLines) > 0 {
					headerSection := Section{
						Title: "Header",
						Properties: map[string]interface{}{
							"Content": strings.Join(headerLines, "\n"),
						},
					}
					sections = append(sections, headerSection)
				}
				inHeader = false
				// Process the current line as the beginning of a section
			} else {
				headerLines = append(headerLines, line)
				continue
			}
		}

		// Start a new section
		if strings.HasPrefix(line, "Handle") {
			if currentSection != nil {
				// If title is empty, use handle value as title
				if currentSection.Title == "" {
					currentSection.Title = currentSection.Handle
				}
				sections = append(sections, *currentSection)
			}
			currentSection = &Section{
				Handle:     strings.TrimPrefix(line, "Handle "),
				Properties: make(map[string]interface{}),
			}
			expectingTitle = true
			currentPropKey = ""
			continue
		}

		// If title is expected, assign the current line as section title
		if expectingTitle {
			currentSection.Title = trimmed
			expectingTitle = false
			continue
		}

		// Process lines with properties
		if colonIndex := strings.Index(trimmed, ":"); colonIndex != -1 {
			key := strings.TrimSpace(trimmed[:colonIndex])
			value := strings.TrimSpace(trimmed[colonIndex+1:])
			if existing, ok := currentSection.Properties[key]; ok {
				// If the property already exists, convert it to an array
				switch v := existing.(type) {
				case []string:
					currentSection.Properties[key] = append(v, value)
				case string:
					currentSection.Properties[key] = []string{v, value}
				default:
					currentSection.Properties[key] = value
				}
			} else {
				currentSection.Properties[key] = value
			}
			currentPropKey = key
		} else {
			// If the line does not contain a colon, assume it is a continuation of the previous property
			if currentPropKey != "" {
				if existing, ok := currentSection.Properties[currentPropKey]; ok {
					if str, ok2 := existing.(string); ok2 {
						currentSection.Properties[currentPropKey] = str + " " + trimmed
					} else if arr, ok2 := existing.([]string); ok2 {
						if len(arr) > 0 {
							arr[len(arr)-1] = arr[len(arr)-1] + " " + trimmed
							currentSection.Properties[currentPropKey] = arr
						} else {
							currentSection.Properties[currentPropKey] = trimmed
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if currentSection != nil {
		// If title is empty, use handle value as title
		if currentSection.Title == "" {
			currentSection.Title = currentSection.Handle
		}
		sections = append(sections, *currentSection)
	}
	return sections, nil
}

// bootctl mounts external EFI partition, copies contents of efishell directory (ctefi)
// and sets one-time boot entry (via setOneTimeBoot). Do not change this function!
func bootctl() error {
	// Determine boot device
	bootDev, err := findBootDevice()
	if err != nil {
		return fmt.Errorf("Could not determine boot device: %v", err)
	}

	debugPrint(fmt.Sprintf("Detected boot device: %s", bootDev))

	// Find external EFI partition
	targetDevice, targetEfi, err := findExternalEfiPartition(bootDev)
	if err != nil || targetDevice == "" || targetEfi == "" {
		return errors.New("No external EFI partition found")
	}

	// Additional check to ensure targetEfi is a partition, not the whole disk
	if targetEfi == targetDevice {
		return fmt.Errorf("targetEfi cannot be the same as targetDevice: %s", targetEfi)
	}

	debugPrint("targetDevice: " + targetDevice)
	debugPrint("targetEFI: " + targetEfi)

	// No need to mount and copy files, as all necessary information is in EFI variables
	debugPrint("Using EFI variables instead of copying files to EFI partition")

	// Call setOneTimeBoot function to create new entry and set BootNext
	if err := setOneTimeBoot(targetDevice, targetEfi); err != nil {
		return fmt.Errorf("setOneTimeBoot error: %v", err)
	}

	if err = runCommandNoOutput("bootctl", "set-oneshot", "03-efishell.conf"); err != nil {
		criticalError("Failed to set one-time boot entry: " + err.Error())
		os.Exit(1)
	} else {
		debugPrint("One-time boot entry set successfully.")
	}

	return nil
}

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func runCommandNoOutput(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// Do not show full output, keep only debug messages
	var dummy bytes.Buffer
	cmd.Stdout = &dummy
	cmd.Stderr = &dummy
	return cmd.Run()
}

func findBootDevice() (string, error) {
	output, err := runCommand("findmnt", "/", "-o", "SOURCE", "-n")
	if err != nil {
		return "", fmt.Errorf("findmnt failed: %v", err)
	}
	output = strings.TrimSpace(output)
	loopRegex := regexp.MustCompile(`^/dev/loop[0-9]+$`)
	if output == "airootfs" || loopRegex.MatchString(output) {
		// If running from ArchISO, check if /run/archiso/bootmnt is mounted
		bootMntSource, err := runCommand("findmnt", "/run/archiso/bootmnt", "-o", "SOURCE", "-n")
		if err == nil && bootMntSource != "" {
			bootMntSource = strings.TrimSpace(bootMntSource)
			debugPrint(fmt.Sprintf("Found archiso boot mount: %s", bootMntSource))

			// Extract the disk device from the partition (e.g. /dev/sda1 -> /dev/sda)
			if strings.Contains(bootMntSource, "nvme") {
				// For NVMe devices: /dev/nvme0n1p1 -> /dev/nvme0n1
				devRegex := regexp.MustCompile(`p[0-9]+$`)
				return devRegex.ReplaceAllString(bootMntSource, ""), nil
			} else {
				// For other devices: /dev/sda1 -> /dev/sda
				devRegex := regexp.MustCompile(`[0-9]+$`)
				return devRegex.ReplaceAllString(bootMntSource, ""), nil
			}
		}
		return "LOOP", nil
	}
	// For NVMe devices, name looks like "/dev/nvme0n1p1" - parent disk: "/dev/nvme0n1"
	if strings.Contains(output, "nvme") {
		devRegex := regexp.MustCompile(`p[0-9]+$`)
		return devRegex.ReplaceAllString(output, ""), nil
	}
	// For other devices, e.g. "/dev/sda2" - parent disk: "/dev/sda"
	devRegex := regexp.MustCompile(`[0-9]+$`)
	return devRegex.ReplaceAllString(output, ""), nil
}

func listRealDisks() ([]string, error) {
	output, err := runCommand("lsblk", "-d", "-o", "NAME,TYPE", "-rn")
	if err != nil {
		return nil, fmt.Errorf("lsblk failed: %v", err)
	}
	var disks []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "disk" {
			disks = append(disks, "/dev/"+fields[0])
		}
	}
	return disks, nil
}

func isEfiPartition(part string) bool {
	output, err := runCommand("blkid", "-o", "export", part)
	if err != nil {
		return false
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if matched, _ := regexp.MatchString(`^TYPE=(fat|vfat|msdos)`, line); matched {
			return true
		}
	}
	return false
}

// Improved function to find external EFI partition with prioritization for the boot device
func findExternalEfiPartition(bootDev string) (string, string, error) {
	disks, err := listRealDisks()
	if err != nil {
		return "", "", err
	}

	debugPrint(fmt.Sprintf("All disks: %v", disks))
	debugPrint(fmt.Sprintf("Boot device: %s", bootDev))

	// Check if we're running from ArchISO/live environment

	// Check what device /run/archiso/bootmnt is mounted on (if we're in a live environment)
	var archisoDev string
	if bootMntSource, err := runCommand("findmnt", "/run/archiso/bootmnt", "-o", "SOURCE", "-n"); err == nil && bootMntSource != "" {
		bootMntSource = strings.TrimSpace(bootMntSource)
		debugPrint(fmt.Sprintf("Found archiso boot mount: %s", bootMntSource))

		// Extract the disk device from the partition (e.g. /dev/sda1 -> /dev/sda)
		if strings.Contains(bootMntSource, "nvme") {
			// For NVMe devices: /dev/nvme0n1p1 -> /dev/nvme0n1
			devRegex := regexp.MustCompile(`p[0-9]+$`)
			archisoDev = devRegex.ReplaceAllString(bootMntSource, "")
		} else {
			// For other devices: /dev/sda1 -> /dev/sda
			devRegex := regexp.MustCompile(`[0-9]+$`)
			archisoDev = devRegex.ReplaceAllString(bootMntSource, "")
		}
		debugPrint(fmt.Sprintf("Extracted archiso device: %s", archisoDev))
	}

	// First check for EFI partitions on the boot device itself (if we're booting from ArchISO)
	var bootDevEfiPartitions []struct {
		disk      string
		partition string
	}

	var otherEfiPartitions []struct {
		disk      string
		partition string
	}

	// First pass - collect all EFI partitions and separate them into boot device partitions and others
	for _, dev := range disks {
		// Determine if this disk is our boot device
		isBootDevice := dev == bootDev || dev == archisoDev

		debugPrint(fmt.Sprintf("Checking disk: %s for partitions (boot device: %v)", dev, isBootDevice))

		// Get all partitions for this disk
		output, err := runCommand("lsblk", "-nlo", "NAME", dev)
		if err != nil {
			debugPrint(fmt.Sprintf("Error listing partitions for %s: %v", dev, err))
			continue
		}

		partitions := strings.Split(output, "\n")
		for _, part := range partitions {
			part = strings.TrimSpace(part)

			// Skip the disk itself from lsblk output
			if part == filepath.Base(dev) {
				continue
			}

			// Construct full path to partition
			partPath := "/dev/" + part
			debugPrint(fmt.Sprintf("Checking partition: %s", partPath))

			// Skip if it's the same as disk device
			if partPath == dev {
				debugPrint(fmt.Sprintf("Skipping partition %s as it's the same as disk device", partPath))
				continue
			}

			if isEfiPartition(partPath) {
				debugPrint(fmt.Sprintf("Found EFI partition: %s on disk: %s", partPath, dev))

				// Add to appropriate list based on whether it's on the boot device
				if isBootDevice {
					bootDevEfiPartitions = append(bootDevEfiPartitions, struct {
						disk      string
						partition string
					}{dev, partPath})
				} else {
					otherEfiPartitions = append(otherEfiPartitions, struct {
						disk      string
						partition string
					}{dev, partPath})
				}
			}
		}
	}

	// First try EFI partitions on the boot device (if any)
	if len(bootDevEfiPartitions) > 0 {
		if len(bootDevEfiPartitions) > 1 {
			debugPrint(fmt.Sprintf("Multiple EFI partitions found on boot device. Using the first one."))
			for i, part := range bootDevEfiPartitions {
				debugPrint(fmt.Sprintf("Boot device EFI partition %d: disk=%s, partition=%s", i+1, part.disk, part.partition))
			}
		}
		debugPrint(fmt.Sprintf("Selected EFI partition on boot device: %s", bootDevEfiPartitions[0].partition))
		return bootDevEfiPartitions[0].disk, bootDevEfiPartitions[0].partition, nil
	}

	// If no EFI partitions found on boot device, fall back to other disks
	if len(otherEfiPartitions) > 0 {
		if len(otherEfiPartitions) > 1 {
			debugPrint(fmt.Sprintf("Multiple EFI partitions found on other devices. Using the first one."))
			for i, part := range otherEfiPartitions {
				debugPrint(fmt.Sprintf("Other device EFI partition %d: disk=%s, partition=%s", i+1, part.disk, part.partition))
			}
		}
		debugPrint(fmt.Sprintf("Selected EFI partition on non-boot device: %s", otherEfiPartitions[0].partition))
		return otherEfiPartitions[0].disk, otherEfiPartitions[0].partition, nil
	}

	// If we get here, no EFI partition was found
	return "", "", errors.New("no EFI partition found on any disk")
}

func getSerialAndMac() error {
	output, err := runCommand("dmidecode", "-t", "system")
	if err != nil {
		return fmt.Errorf("dmidecode failed: %v", err)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Product Name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				productName = strings.TrimSpace(parts[1])
				break
			}
		}
	}
	if productName == "" {
		return errors.New("Could not determine Product Name. Make sure dmidecode is run with sufficient privileges.")
	}
	fmt.Printf("Product Name: %s\n", productName)

	// Determine MAC flashing method based on product type
	requiredFields := map[string]*regexp.Regexp{}

	switch productName {
	case "Silver":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A3[0-9]{8}$`)
		requiredFields["ioSN"] = regexp.MustCompile(`^INF0[0-9]{1}A4[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFMBH610MTPR":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A9[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFMBB760M":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B4[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFMBH770M-PRO":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B5[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFAIOI5IP":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A1[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "Mercury":
		macFlashingMethod = "rtnicpg"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B7[0-9]{8}$`) // INF00B722250002
		requiredFields["ioSN"] = regexp.MustCompile(`^INF0[0-9]{1}B8[0-9]{8}$`) // INF00B822250002
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "SP2C621D32TM3":
		macFlashingMethod = "eeupdate"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A9[0-9]{8}$`) //INF00A903250001
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "RS224":
		macFlashingMethod = "eeupdate"
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B2[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	default:
		return fmt.Errorf("Unknown product name: %s", productName)
	}

	fmt.Println("Please enter the following values (the program will automatically detect the type):")
	for key, regex := range requiredFields {
		fmt.Printf(" - %s (expected format: %s)\n", key, regex.String())
	}

	provided := make(map[string]string)
	reader := bufio.NewReader(os.Stdin)

	for len(provided) < len(requiredFields) {
		fmt.Print("Enter value: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		input = strings.TrimSpace(input)
		if input == "" {
			fmt.Println("Input cannot be empty. Please re-enter.")
			continue
		}
		matched := false
		for key, regex := range requiredFields {
			if _, ok := provided[key]; ok {
				continue
			}
			if regex.MatchString(input) {
				provided[key] = input
				fmt.Printf("%s value accepted: %s\n", key, input)
				matched = true
				break
			}
		}
		if !matched {
			fmt.Println("Input does not match any expected format. Please try again.")
		}
	}

	// Wait for extra (4th) line input, but no more than 500 ms.
	if _, err := readLineWithTimeout(500 * time.Millisecond); err != nil {
		debugPrint("No extra input received within 500ms, proceeding...")
	}

	if val, ok := provided["mbSN"]; ok {
		mbSN = val
	}
	if val, ok := provided["ioSN"]; ok {
		ioSN = val
	}
	if val, ok := provided["mac"]; ok {
		mac = val
	}

	fmt.Println("Collected data:")
	fmt.Printf("  mbSN: %s\n", mbSN)
	if productName == "Silver" {
		fmt.Printf("  ioSN: %s\n", ioSN)
	}
	fmt.Printf("  MAC: %s\n", mac)
	debugPrint(fmt.Sprintf("Using MAC flashing method: %s", macFlashingMethod))
	return nil
}

// readLineWithTimeout tries to read a line from os.Stdin with a given timeout.
// Sets non-blocking mode on descriptor and performs cyclic check.
func readLineWithTimeout(timeout time.Duration) (string, error) {
	fd := int(os.Stdin.Fd())
	// Set non-blocking mode.
	if err := syscall.SetNonblock(fd, true); err != nil {
		return "", err
	}
	// Restore blocking mode when done.
	defer syscall.SetNonblock(fd, false)

	reader := bufio.NewReader(os.Stdin)
	deadline := time.Now().Add(timeout)
	for {
		// If there's at least one byte, read the whole line.
		_, err := reader.Peek(1)
		if err == nil {
			return reader.ReadString('\n')
		}
		// If time is up, stop waiting.
		if time.Now().After(deadline) {
			return "", errors.New("timeout reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getSystemSerial(dmiType string) (string, error) {
	out, err := runCommand("dmidecode", "-t", dmiType)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Serial Number:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", errors.New("Serial Number not found")
}

// incrementMAC increases MAC address by 1
func incrementMAC(mac string) (string, error) {
	// Split MAC address into bytes
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return "", fmt.Errorf("invalid MAC address format: %s", mac)
	}

	// Convert the last byte to an integer, increment it, and convert back
	lastByte, err := strconv.ParseUint(parts[5], 16, 8)
	if err != nil {
		return "", fmt.Errorf("invalid MAC address byte: %s", parts[5])
	}

	// Increment with overflow handling
	lastByte = (lastByte + 1) % 256

	// If the last byte overflows, increment the previous byte
	if lastByte == 0 {
		fifthByte, err := strconv.ParseUint(parts[4], 16, 8)
		if err != nil {
			return "", fmt.Errorf("invalid MAC address byte: %s", parts[4])
		}
		fifthByte = (fifthByte + 1) % 256
		parts[4] = fmt.Sprintf("%02x", fifthByte)
	}

	// Update the last byte
	parts[5] = fmt.Sprintf("%02x", lastByte)

	// Join parts back together
	return strings.Join(parts, ":"), nil
}

func getInterfacesWithMAC(targetMAC string) ([]string, error) {
	output, err := runCommand("ip", "-o", "link", "show")
	if err != nil {
		return nil, fmt.Errorf("Failed to get ip link show: %v", err)
	}
	re := regexp.MustCompile(`^\d+:\s+([^:]+):.*link/ether\s+([0-9a-f:]+)`)
	var interfaces []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		matches := re.FindStringSubmatch(line)
		if len(matches) == 3 {
			iface := matches[1]
			macFound := matches[2]
			if strings.ToLower(macFound) == strings.ToLower(targetMAC) {
				interfaces = append(interfaces, iface)
			}
		}
	}
	if len(interfaces) == 0 {
		return nil, fmt.Errorf("No interface found with MAC address: %s", targetMAC)
	}
	return interfaces, nil
}

// getCompiledDriverDirectory returns the directory for finding/saving compiled driver files
// This is specified by the -driver parameter
func getCompiledDriverDirectory() string {
	if driverDir != "" {
		// Create directory if it doesn't exist
		if _, err := os.Stat(driverDir); os.IsNotExist(err) {
			if err := os.MkdirAll(driverDir, 0755); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Could not create driver directory %s: %v\n"+colorReset, driverDir, err)
				// Fall back to the current working directory
				fallbackDir := filepath.Join(cDir, "compiled_drivers")
				_ = os.MkdirAll(fallbackDir, 0755) // Try to create it, ignore errors
				return fallbackDir
			}
		}
		return driverDir
	}

	// Use a default directory in the current working directory
	defaultDir := filepath.Join(cDir, "compiled_drivers")
	_ = os.MkdirAll(defaultDir, 0755) // Try to create it, ignore errors
	return defaultDir
}

// getRtnicpgDirectory returns the directory containing rtnicpg source code
func getRtnicpgDirectory() string {
	return filepath.Join(tempRtnicpgPath, "rtnicpg")
}

func getActiveInterfaceAndIP() (string, string, error) {
	output, err := runCommand("ip", "a")
	if err != nil {
		return "", "", fmt.Errorf("Failed to get 'ip a' output: %v", err)
	}

	lines := strings.Split(output, "\n")
	var currentIface, currentIP string
	headerRe := regexp.MustCompile(`^\d+:\s+([^:]+):\s+<([^>]+)>`)
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if matches := headerRe.FindStringSubmatch(line); len(matches) == 3 {
			ifaceName := matches[1]
			flags := matches[2]
			if ifaceName == "lo" {
				continue
			}
			if !strings.Contains(flags, "UP") {
				continue
			}
			currentIface = ifaceName
			for j := i + 1; j < len(lines); j++ {
				nextLine := strings.TrimSpace(lines[j])
				if nextLine == "" {
					continue
				}
				if headerRe.MatchString(nextLine) {
					break
				}
				if strings.HasPrefix(nextLine, "inet ") {
					fields := strings.Fields(nextLine)
					if len(fields) >= 2 {
						currentIP = fields[1]
						break
					}
				}
			}
			if currentIP != "" {
				break
			}
		}
	}

	if currentIface == "" {
		return "", "", errors.New("no active interface found")
	}
	if currentIP == "" {
		return currentIface, "", errors.New("active interface found but no IPv4 address detected")
	}
	return currentIface, currentIP, nil
}

func loadDriver() error {
	moduleDefault := "pgdrv"
	modulesToRemove := []string{"r8169", "r8168", "r8125", "r8101"}

	// Get the compiled driver storage directory (from -driver flag)
	compiledDriverDir := getCompiledDriverDirectory()
	// Get the source directory for compilation (only used if needed)
	srcDir := getRtnicpgDirectory()

	debugPrint(fmt.Sprintf("Checking compiled driver directory: %s", compiledDriverDir))
	if driverDir != "" {
		debugPrint(fmt.Sprintf("Using driver directory from -driver flag: %s", driverDir))
	}

	// Remove conflicting modules first
	for _, mod := range modulesToRemove {
		if isModuleLoaded(mod) {
			fmt.Printf("Removing module: %s\n", mod)
			if err := runCommandNoOutput("rmmod", mod); err != nil {
				fmt.Printf("[WARNING] Could not remove module %s: %v\n", mod, err)
			} else {
				fmt.Printf("[INFO] Module %s successfully removed.\n", mod)
				rtDrv = mod
			}
		}
	}

	var targetModule string
	// Get kernel version to include in driver filename
	kernelVersion, err := runCommand("uname", "-r")
	if err != nil {
		kernelVersion = "unknown"
	} else {
		kernelVersion = strings.TrimSpace(kernelVersion)
	}
	if rtDrv != "" {
		targetModule = rtDrv + "_mod_" + kernelVersion + ".ko"
	} else {
		targetModule = moduleDefault + ".ko"
	}
	targetModulePath := filepath.Join(compiledDriverDir, targetModule)

	// FIRST CHECK: Does compiled driver already exist in the -driver directory?
	if _, err := os.Stat(targetModulePath); err == nil {
		debugPrint(fmt.Sprintf("Found existing compiled driver at %s", targetModulePath))
		fmt.Printf(colorGreen+"[INFO] Found existing driver file %s. Loading it...\n"+colorReset, targetModulePath)
		modName := strings.TrimSuffix(targetModule, ".ko")
		if isModuleLoaded(modName) {
			fmt.Printf(colorGreen+"[INFO] Module %s is already loaded.\n"+colorReset, modName)
			return nil
		}
		if err := runCommandNoOutput("insmod", targetModulePath); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to load existing module %s: %v\n"+colorReset, targetModulePath, err)
			// Don't return error here, we'll try to compile from source
			fmt.Printf("[INFO] Will try to recompile the driver from source\n")
		} else {
			fmt.Printf(colorGreen+"[INFO] Module %s loaded successfully from compiled driver directory.\n"+colorReset, targetModule)
			return nil
		}
	} else {
		debugPrint(fmt.Sprintf("No existing compiled driver found at %s (err: %v)", targetModulePath, err))
	}

	// If we get here, either:
	// 1. The driver wasn't in the -driver directory
	// 2. Or it was there but failed to load
	// So we need to compile from source

	// Check if source directory exists
	if info, err := os.Stat(srcDir); err != nil || !info.IsDir() {
		return fmt.Errorf("rtnicpg source directory %s does not exist or is not a directory", srcDir)
	}

	// Now compile from source
	fmt.Printf("[INFO] Compiling module %s from source directory %s\n", moduleDefault, srcDir)
	cmd := exec.Command("make", "-C", srcDir, "clean", "all")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		debugPrint(fmt.Sprintf("Compilation output: %s", output.String()))
		return fmt.Errorf("Compilation failed: %v", err)
	}
	debugPrint(fmt.Sprintf("Compilation output: %s", output.String()))
	fmt.Println(colorGreen + "[INFO] Compilation completed successfully." + colorReset)

	// Find the built module in the source directory
	builtModule := filepath.Join(srcDir, moduleDefault+".ko")
	if _, err := os.Stat(builtModule); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Compiled module %s not found", builtModule)
	}

	// Create the directory structure if needed
	if err := os.MkdirAll(filepath.Dir(targetModulePath), 0755); err != nil {
		return fmt.Errorf("Failed to create directory for %s: %v", targetModulePath, err)
	}

	// Save the compiled driver to the storage directory
	data, err := os.ReadFile(builtModule)
	if err != nil {
		return fmt.Errorf("Failed to read compiled module %s: %v", builtModule, err)
	}

	if err := os.WriteFile(targetModulePath, data, 0644); err != nil {
		return fmt.Errorf("Failed to save compiled module to %s: %v", targetModulePath, err)
	}

	fmt.Printf(colorGreen+"[INFO] Saved newly compiled module from %s to %s\n"+colorReset, builtModule, targetModulePath)

	// Load the newly compiled and saved module
	if err := runCommandNoOutput("insmod", targetModulePath); err != nil {
		return fmt.Errorf("Failed to load newly compiled module %s: %v", targetModulePath, err)
	}
	fmt.Printf(colorGreen+"[INFO] Newly compiled module %s loaded successfully.\n"+colorReset, targetModulePath)
	return nil
}

// isModuleLoaded checks if a kernel module is already loaded
func isModuleLoaded(mod string) bool {
	out, err := runCommand("lsmod")
	if err != nil {
		return false
	}
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == mod {
			return true
		}
	}
	return false
}

// setOneTimeBoot creates a new one-time boot entry and sets BootNext
func setOneTimeBoot(targetDevice, targetEfi string) error {
	debugPrint(fmt.Sprintf("setOneTimeBoot: targetDevice=%s, targetEfi=%s", targetDevice, targetEfi))

	// Use the regular expression that should not be changed - DO NOT TOUCH!
	re := regexp.MustCompile(`(?im)^Boot([0-9A-Fa-f]{4})(\*?)\s+OneTimeBoot\t(.+)$`)

	// Check if there are conflicting entries
	out, err := runCommand("efibootmgr")
	if err != nil {
		return fmt.Errorf("efibootmgr failed: %v", err)
	}

	// Find only entries that conflict (have the same boot path)
	matches := re.FindAllStringSubmatch(out, -1)

	// Define the boot path for our new entry
	targetBootPath := "\\EFI\\BOOT\\shellx64.efi -delay:0"

	// Determine partition number for the new device
	var partition string

	// Extract the partition number from targetEfi path
	if strings.Contains(targetDevice, "nvme") {
		// For NVMe devices, name looks like "/dev/nvme0n1p1" - parent disk: "/dev/nvme0n1"
		// Verify that targetEfi has format like /dev/nvme0n1p1
		nvmePartRegex := regexp.MustCompile(`^(/dev/nvme[0-9]+n[0-9]+)p([0-9]+)$`)
		matches := nvmePartRegex.FindStringSubmatch(targetEfi)
		if len(matches) == 3 {
			debugPrint(fmt.Sprintf("NVMe partition identified: disk=%s, partition=%s", matches[1], matches[2]))
			// Check if targetDevice matches the disk part
			if matches[1] != targetDevice {
				debugPrint(fmt.Sprintf("Warning: Extracted disk %s doesn't match targetDevice %s", matches[1], targetDevice))
				// Use the matched disk as targetDevice for consistency
				targetDevice = matches[1]
			}
			partition = matches[2]
		} else {
			return fmt.Errorf("invalid NVMe partition format: %s", targetEfi)
		}
	} else {
		// For other devices, e.g. "/dev/sda1" - parent disk: "/dev/sda"
		stdPartRegex := regexp.MustCompile(`^(/dev/[a-z]+)([0-9]+)$`)
		matches := stdPartRegex.FindStringSubmatch(targetEfi)
		if len(matches) == 3 {
			debugPrint(fmt.Sprintf("Standard partition identified: disk=%s, partition=%s", matches[1], matches[2]))
			// Check if targetDevice matches the disk part
			if matches[1] != targetDevice {
				debugPrint(fmt.Sprintf("Warning: Extracted disk %s doesn't match targetDevice %s", matches[1], targetDevice))
				// Use the matched disk as targetDevice for consistency
				targetDevice = matches[1]
			}
			partition = matches[2]
		} else {
			return fmt.Errorf("invalid partition format: %s", targetEfi)
		}
	}

	if partition == "" {
		return fmt.Errorf("could not determine partition number from targetEfi: %s", targetEfi)
	}

	debugPrint(fmt.Sprintf("Using disk device: %s, partition: %s", targetDevice, partition))

	// Remove only entries that conflict with our target entry
	for _, match := range matches {
		bootNum := match[1]

		// Get more detailed info about the entry
		bootInfo, err := runCommand("efibootmgr", "-v", "-b", bootNum)
		if err != nil {
			debugPrint(fmt.Sprintf("[WARNING] Failed to get info for Boot%s: %v", bootNum, err))
			continue
		}

		// Check if the entry contains the same boot path
		if strings.Contains(bootInfo, targetBootPath) {
			debugPrint("[INFO] Removing conflicting OneTimeBoot entry: Boot" + bootNum)
			if err := runCommandNoOutput("efibootmgr", "-B", "-b", bootNum); err != nil {
				debugPrint(fmt.Sprintf("[WARNING] Failed to remove Boot%s: %v", bootNum, err))
			}
		} else {
			debugPrint("[INFO] Keeping non-conflicting OneTimeBoot entry: Boot" + bootNum)
		}
	}

	debugPrint("targetDevice: " + targetDevice)
	debugPrint("Partition: " + partition)

	debugPrint("[INFO] Creating new OneTimeBoot entry")
	// Create a new entry without displaying command result
	createCmd := exec.Command("efibootmgr",
		"-c",
		"-d", targetDevice,
		"-p", partition,
		"-L", "OneTimeBoot",
		"-l", targetBootPath)
	// Hide efibootmgr output, keep only debug messages
	var createOut bytes.Buffer
	createCmd.Stdout = &createOut
	createCmd.Stderr = &createOut
	if err := createCmd.Run(); err != nil {
		debugPrint("[ERROR] efibootmgr create output: " + createOut.String())
		return fmt.Errorf("failed to create new boot entry: %v", err)
	}

	// Find the created entry with OneTimeBoot label
	out, err = runCommand("efibootmgr", "-v")
	if err != nil {
		return fmt.Errorf("efibootmgr failed after creation: %v", err)
	}
	matches = re.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return errors.New("new OneTimeBoot entry not found after creation")
	}

	// Find our new entry - it should be the last created with this label
	var bootNum string
	for _, match := range matches {
		candidateBootNum := match[1]
		bootInfo, err := runCommand("efibootmgr", "-v", "-b", candidateBootNum)
		if err == nil && strings.Contains(bootInfo, targetBootPath) &&
			strings.Contains(bootInfo, targetDevice) {
			bootNum = candidateBootNum
			break
		}
	}

	if bootNum == "" {
		// If we didn't find an exact match, use the last entry
		bootNum = matches[len(matches)-1][1]
	}

	debugPrint("[INFO] New OneTimeBoot entry created: Boot" + bootNum)

	// Set BootNext to the created entry
	if err := runCommandNoOutput("efibootmgr", "-n", bootNum); err != nil {
		out2, err2 := runCommand("efibootmgr", "-v")
		if err2 == nil && strings.Contains(out2, "BootNext: "+bootNum) {
			debugPrint("BootNext is already set to Boot" + bootNum)
			return nil
		}
		return fmt.Errorf("failed to set BootNext to %s: %v", bootNum, err)
	}

	out3, err3 := runCommand("efibootmgr", "-v")
	if err3 == nil && strings.Contains(out3, "BootNext: "+bootNum) {
		debugPrint("BootNext is set to Boot" + bootNum)
		return nil
	}

	return fmt.Errorf("failed to verify BootNext setting for Boot%s", bootNum)
}

// Обновленная функция detectIntelNetworkCards с более надежным запуском eeupdate64e
func detectIntelNetworkCards() ([]string, error) {
	debugPrint("Detecting Intel network cards using eeupdate64e...")

	// Определяем пути к директории и исполняемому файлу eeupdate64e
	eeupdate64eDir := filepath.Join(tempEeupdatePath, "eeupdate")
	eeupdate64ePath := filepath.Join(eeupdate64eDir, "eeupdate64e")

	// Проверяем наличие файла
	if _, err := os.Stat(eeupdate64ePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("eeupdate64e not found at path %s: %v", eeupdate64ePath, err)
	}

	// Устанавливаем права на исполнение
	if err := os.Chmod(eeupdate64ePath, 0755); err != nil {
		debugPrint(fmt.Sprintf("Warning: Failed to set executable permissions for %s: %v", eeupdate64ePath, err))
	}

	// Сохраняем текущую директорию
	originalDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current directory: %v", err)
	}

	// Переходим в директорию eeupdate64e
	if err := os.Chdir(eeupdate64eDir); err != nil {
		return nil, fmt.Errorf("failed to change to eeupdate directory: %v", err)
	}

	// Убедимся, что вернемся в исходную директорию в любом случае
	defer os.Chdir(originalDir)

	// Проверяем работоспособность eeupdate64e с простым выводом помощи
	helpOutput, helpErr := exec.Command("./eeupdate64e", "/h").CombinedOutput()
	if helpErr != nil {
		debugPrint(fmt.Sprintf("Warning: eeupdate64e help command failed: %v\nOutput: %s", helpErr, string(helpOutput)))
		// Продолжаем даже при ошибке, так как некоторые версии могут выдавать ошибку при выводе помощи
	}

	// Пробуем выполнить команду MAC_DUMP_ALL для обнаружения карт
	cmd := exec.Command("./eeupdate64e", "/MAC_DUMP_ALL")
	output, err := cmd.CombinedOutput()
	if err != nil {
		debugPrint(fmt.Sprintf("Warning: eeupdate64e MAC_DUMP_ALL failed: %v\nOutput: %s", err, string(output)))

		// Проверяем наличие фразы о неустановленном QV драйвере
		if strings.Contains(string(output), "Connection to QV driver failed") {
			debugPrint("QV driver not installed or not loaded. Trying driverless mode...")

			// В режиме без драйвера проверяем каждую карту отдельно
			return detectIntelCardsOneByOne(eeupdate64eDir)
		}

		return nil, fmt.Errorf("failed to detect network cards: %v", err)
	}

	// Анализируем вывод команды MAC_DUMP_ALL
	var cards []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Ищем строки с информацией о сетевых картах
		if strings.Contains(line, "NIC #") && strings.Contains(line, "MAC Address:") {
			nicMatch := regexp.MustCompile(`NIC #(\d+)`).FindStringSubmatch(line)
			if len(nicMatch) > 1 {
				cards = append(cards, fmt.Sprintf("NIC %s", nicMatch[1]))
			} else {
				// Если номер не найден, используем порядковый номер
				cards = append(cards, fmt.Sprintf("NIC %d", len(cards)+1))
			}
		}
	}

	// Если карты не найдены через MAC_DUMP_ALL, попробуем другой метод
	if len(cards) == 0 {
		return detectIntelCardsOneByOne(eeupdate64eDir)
	}

	debugPrint(fmt.Sprintf("Detected %d Intel network cards", len(cards)))
	return cards, nil
}

// Вспомогательная функция для обнаружения карт по одной
func detectIntelCardsOneByOne(eeupdateDir string) ([]string, error) {
	var cards []string

	debugPrint("Trying to detect Intel network cards one by one...")

	// Проверяем до 8 карт
	for i := 1; i <= 8; i++ {
		cmd := exec.Command("./eeupdate64e", "/NIC="+fmt.Sprintf("%d", i), "/MAC_DUMP")
		cmd.Dir = eeupdateDir
		output, err := cmd.CombinedOutput()
		outputStr := string(output)

		// Выводим подробную отладочную информацию
		debugPrint(fmt.Sprintf("NIC %d detection result: %v\nOutput: %s", i, err, outputStr))

		// Игнорируем сообщение о QV драйвере, это нормально
		// Проверяем наличие конкретных признаков успешного обнаружения карты

		// Проверка на явное сообщение об отсутствии карты
		if strings.Contains(outputStr, "Error: Nic not found") ||
			strings.Contains(outputStr, "No Intel(R) PRO Adapters found") {
			debugPrint(fmt.Sprintf("NIC %d not found, stopping search", i))
			break // Прекращаем поиск, если карта не найдена
		}

		// Проверка на наличие MAC-адреса в выводе, что указывает на обнаруженную карту
		macFound := false
		lines := strings.Split(outputStr, "\n")
		for _, line := range lines {
			if strings.Contains(line, "LAN MAC Address is") ||
				strings.Contains(line, "MAC Address:") {
				macFound = true
				break
			}
		}

		if macFound {
			cards = append(cards, fmt.Sprintf("NIC %d", i))
			debugPrint(fmt.Sprintf("Found valid NIC %d with MAC address", i))
		} else if strings.Contains(outputStr, "Intel(R)") &&
			strings.Contains(outputStr, "Network Connection") {
			// Дополнительная проверка: есть упоминание сетевой карты Intel в выводе
			cards = append(cards, fmt.Sprintf("NIC %d", i))
			debugPrint(fmt.Sprintf("Found valid NIC %d (Intel Network Connection)", i))
		} else {
			debugPrint(fmt.Sprintf("NIC %d appears invalid, stopping search", i))
			break
		}
	}

	if len(cards) == 0 {
		return nil, fmt.Errorf("no Intel network cards detected")
	}

	debugPrint(fmt.Sprintf("Detected %d Intel network cards", len(cards)))
	return cards, nil
}

// Обновленная функция writeMAcWithEeupdate для правильного запуска eeupdate64e
func writeMAcWithEeupdate(macAddress string, nicIndex int) error {
	debugPrint(fmt.Sprintf("Writing MAC %s to network card #%d using eeupdate64e...", macAddress, nicIndex))
	macAddress = strings.ReplaceAll(macAddress, ":", "")

	// Определяем пути к директории и исполняемому файлу eeupdate64e
	eeupdate64eDir := filepath.Join(tempEeupdatePath, "eeupdate")
	eeupdate64ePath := filepath.Join(eeupdate64eDir, "eeupdate64e")

	// Проверяем наличие файла
	if _, err := os.Stat(eeupdate64ePath); os.IsNotExist(err) {
		return fmt.Errorf("eeupdate64e not found at path %s: %v", eeupdate64ePath, err)
	}

	// Сохраняем текущую директорию
	originalDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %v", err)
	}

	// Переходим в директорию eeupdate64e
	if err := os.Chdir(eeupdate64eDir); err != nil {
		return fmt.Errorf("failed to change to eeupdate directory: %v", err)
	}

	// Убедимся, что вернемся в исходную директорию в любом случае
	defer os.Chdir(originalDir)

	// Форматируем команду с индексом NIC
	args := []string{"/NIC=" + fmt.Sprintf("%d", nicIndex), "/MAC=" + macAddress}

	// Запускаем eeupdate64e для прошивки MAC-адреса
	cmd := exec.Command("./eeupdate64e", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("eeupdate64e failed to write MAC: %v\nOutput: %s", err, string(output))
	}

	// Проверяем вывод на наличие сообщений об ошибках
	if strings.Contains(string(output), "Error") || strings.Contains(string(output), "Failed") {
		return fmt.Errorf("eeupdate64e reported an error: %s", string(output))
	}

	debugPrint(fmt.Sprintf("MAC %s successfully written to network card #%d", macAddress, nicIndex))
	return nil
}

func writeAllMACsToEfiVars(macAddresses []string) error {
	if len(macAddresses) == 0 {
		return fmt.Errorf("no MAC addresses provided")
	}

	debugPrint(fmt.Sprintf("Writing %d MAC addresses to EFI variables", len(macAddresses)))

	// Генерируем GUID для EFI-переменных, если еще не сгенерирован
	var err error
	if efiVarGUID == "" {
		efiVarGUID, err = randomGUIDWithPrefix(guidPrefix)
		if err != nil {
			return fmt.Errorf("failed to generate GUID: %v", err)
		}
		debugPrint("Generated EFI variable GUID: " + efiVarGUID)
	}

	// Записываем первый MAC-адрес в переменную без индекса (для совместимости)
	if err := writeMACToEfiVar(macAddresses[0], 0); err != nil {
		return fmt.Errorf("failed to write primary MAC to EFI variable: %v", err)
	}

	// Записываем все MAC-адреса, начиная с первого, в нумерованные переменные
	for i, mac := range macAddresses {
		// i+1 потому что нумерация начинается с 1, но первый MAC уже записан выше
		if err := writeMACToEfiVar(mac, i+1); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to write MAC #%d to EFI variable: %v\n"+colorReset, i+1, err)
			// Продолжаем с другими MAC-адресами, не останавливаемся при ошибке
		}
	}

	return nil
}

// getIPAddresses gets all IP addresses for a given interface
// Used for checking existing addresses during restoration
func getIPAddresses(iface, ipVersion string) ([]string, error) {
	var addresses []string

	// Run ip command to get addresses
	output, err := runCommand("ip", ipVersion, "addr", "show", "dev", iface)
	if err != nil {
		return nil, fmt.Errorf("failed to get IP addresses for %s: %v", iface, err)
	}

	// Parse output based on IP version
	var re *regexp.Regexp
	if ipVersion == "-4" {
		re = regexp.MustCompile(`inet\s+([0-9.]+/[0-9]+)`)
	} else {
		re = regexp.MustCompile(`inet6\s+([0-9a-f:]+/[0-9]+)`)
	}

	// Find all matches
	matches := re.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		if len(match) >= 2 {
			addresses = append(addresses, match[1])
		}
	}

	return addresses, nil
} // Add these new functions to support igb driver restart

// identifyIgbInterfaces discovers network interfaces that use the igb driver
func identifyIgbInterfaces() ([]string, error) {
	var igbInterfaces []string

	// First, get a list of all network interfaces
	output, err := runCommand("ls", "/sys/class/net")
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %v", err)
	}

	// Check each interface for igb driver
	interfaces := strings.Fields(output)
	for _, iface := range interfaces {
		// Skip loopback interface
		if iface == "lo" {
			continue
		}

		// Check if this interface uses the igb driver
		driverPath := fmt.Sprintf("/sys/class/net/%s/device/driver", iface)

		// First check if the path exists
		if _, err := os.Stat(driverPath); os.IsNotExist(err) {
			// Path doesn't exist, so skip this interface
			continue
		}

		// Read the driver symlink
		driverLink, err := os.Readlink(driverPath)
		if err != nil {
			// If we can't read the link, just continue to the next interface
			debugPrint(fmt.Sprintf("Could not read driver symlink for %s: %v", iface, err))
			continue
		}

		// Check if this is an igb driver
		if strings.Contains(driverLink, "/igb") {
			igbInterfaces = append(igbInterfaces, iface)
			debugPrint(fmt.Sprintf("Found interface %s using igb driver", iface))
		}
	}

	return igbInterfaces, nil
}

// IPAddrInfo stores information about an IP address
type IPAddrInfo struct {
	Address     string
	IsDynamic   bool
	IsSecondary bool
	Scope       string
	Metric      string
	Line        string // Full line from ip addr command
}

// InterfaceIPInfo stores IP configuration for an interface
type InterfaceIPInfo struct {
	Name      string
	IPv4Addrs []IPAddrInfo
	IPv6Addrs []IPAddrInfo
	Up        bool
}

// captureInterfaceIPs saves the current IP configuration of specified interfaces
func captureInterfaceIPs(interfaces []string) ([]InterfaceIPInfo, error) {
	var result []InterfaceIPInfo

	for _, iface := range interfaces {
		info := InterfaceIPInfo{
			Name: iface,
		}

		// Get interface status (up/down)
		statusOutput, err := runCommand("ip", "link", "show", "dev", iface)
		if err == nil {
			info.Up = strings.Contains(statusOutput, "state UP")
		}

		// Get IPv4 addresses with full details
		ipv4Output, err := runCommand("ip", "-4", "addr", "show", "dev", iface)
		if err == nil {
			// Process each line of the output
			lines := strings.Split(ipv4Output, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "inet ") {
					addrInfo := IPAddrInfo{
						Line: line,
					}

					// Extract the address with CIDR
					addrMatch := regexp.MustCompile(`inet\s+([0-9.]+/[0-9]+)`).FindStringSubmatch(line)
					if len(addrMatch) >= 2 {
						addrInfo.Address = addrMatch[1]
					} else {
						continue
					}

					// Check if it's dynamic (from DHCP)
					addrInfo.IsDynamic = strings.Contains(line, "dynamic")

					// Check if it's secondary
					addrInfo.IsSecondary = strings.Contains(line, "secondary")

					// Extract scope
					scopeMatch := regexp.MustCompile(`scope\s+(\w+)`).FindStringSubmatch(line)
					if len(scopeMatch) >= 2 {
						addrInfo.Scope = scopeMatch[1]
					}

					// Extract metric if present
					metricMatch := regexp.MustCompile(`metric\s+(\d+)`).FindStringSubmatch(line)
					if len(metricMatch) >= 2 {
						addrInfo.Metric = metricMatch[1]
					}

					info.IPv4Addrs = append(info.IPv4Addrs, addrInfo)
					debugPrint(fmt.Sprintf("Captured IPv4: %s (dynamic: %v, secondary: %v, scope: %s, metric: %s)",
						addrInfo.Address, addrInfo.IsDynamic, addrInfo.IsSecondary, addrInfo.Scope, addrInfo.Metric))
				}
			}
		}

		// Get IPv6 addresses with full details
		ipv6Output, err := runCommand("ip", "-6", "addr", "show", "dev", iface)
		if err == nil {
			// Process each line of the output
			lines := strings.Split(ipv6Output, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "inet6 ") {
					addrInfo := IPAddrInfo{
						Line: line,
					}

					// Extract the address with CIDR
					addrMatch := regexp.MustCompile(`inet6\s+([0-9a-f:]+/[0-9]+)`).FindStringSubmatch(line)
					if len(addrMatch) >= 2 {
						addrInfo.Address = addrMatch[1]
					} else {
						continue
					}

					// Check if it's dynamic
					addrInfo.IsDynamic = strings.Contains(line, "dynamic") || strings.Contains(line, "kernel_ll")

					// Extract scope
					scopeMatch := regexp.MustCompile(`scope\s+(\w+)`).FindStringSubmatch(line)
					if len(scopeMatch) >= 2 {
						addrInfo.Scope = scopeMatch[1]
					}

					info.IPv6Addrs = append(info.IPv6Addrs, addrInfo)
					debugPrint(fmt.Sprintf("Captured IPv6: %s (dynamic: %v, scope: %s)",
						addrInfo.Address, addrInfo.IsDynamic, addrInfo.Scope))
				}
			}
		}

		result = append(result, info)
		debugPrint(fmt.Sprintf("Captured all IP config for %s (Up: %v)", info.Name, info.Up))
	}

	return result, nil
}

// restoreInterfaceIPs restores the IP configuration to interfaces
func restoreInterfaceIPs(interfacesInfo []InterfaceIPInfo) error {
	for _, info := range interfacesInfo {
		// First check if the interface exists
		_, err := runCommand("ip", "link", "show", "dev", info.Name)
		if err != nil {
			fmt.Printf(colorYellow+"[WARNING] Interface %s no longer exists, skipping restoration\n"+colorReset, info.Name)
			continue
		}

		debugPrint(fmt.Sprintf("Restoring interface %s (Up: %v)", info.Name, info.Up))

		// Bring interface down
		_ = runCommandNoOutput("ip", "link", "set", "dev", info.Name, "down")
		time.Sleep(500 * time.Millisecond) // Give the system time to process the change

		// Check current addresses before flushing
		currentAddr, _ := runCommand("ip", "addr", "show", "dev", info.Name)
		debugPrint(fmt.Sprintf("Current addresses before flush:\n%s", currentAddr))

		// Thoroughly flush all addresses
		if err := runCommandNoOutput("ip", "addr", "flush", "dev", info.Name); err != nil {
			debugPrint(fmt.Sprintf("Warning: Error flushing addresses: %v", err))
		}
		time.Sleep(500 * time.Millisecond) // Give the system time to process the flush

		// Double-check if addresses were flushed properly
		postFlushAddr, _ := runCommand("ip", "addr", "show", "dev", info.Name)
		debugPrint(fmt.Sprintf("Addresses after flush:\n%s", postFlushAddr))

		// Get any addresses that survived the flush
		postFlushIPv4, _ := getIPAddresses(info.Name, "-4")
		postFlushIPv6, _ := getIPAddresses(info.Name, "-6")

		// Remove any addresses that survived the flush
		for _, addr := range postFlushIPv4 {
			_ = runCommandNoOutput("ip", "-4", "addr", "del", addr, "dev", info.Name)
		}
		for _, addr := range postFlushIPv6 {
			_ = runCommandNoOutput("ip", "-6", "addr", "del", addr, "dev", info.Name)
		}

		// Restore ALL original IPv4 addresses (including dynamic ones)
		// In reverse order to put primary addresses first
		for i := len(info.IPv4Addrs) - 1; i >= 0; i-- {
			addr := info.IPv4Addrs[i]

			// Skip addresses with empty Address field
			if addr.Address == "" {
				continue
			}

			var ipArgs []string

			// Start with basic arguments
			ipArgs = []string{"-4", "addr", "add", addr.Address, "dev", info.Name}

			// Add scope if present
			if addr.Scope != "" {
				ipArgs = append(ipArgs, "scope", addr.Scope)
			}

			// Add metric if present
			if addr.Metric != "" {
				ipArgs = append(ipArgs, "metric", addr.Metric)
			}

			debugPrint(fmt.Sprintf("Running: ip %s", strings.Join(ipArgs, " ")))

			if err := runCommandNoOutput("ip", ipArgs...); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to restore IPv4 address %s to %s: %v\n"+colorReset,
					addr.Address, info.Name, err)
			} else {
				debugPrint(fmt.Sprintf("Restored IPv4 address %s to %s", addr.Address, info.Name))
			}
		}

		// Restore ALL original IPv6 addresses (except kernel_ll which regenerates automatically)
		for _, addr := range info.IPv6Addrs {
			// Skip addresses with empty Address field
			if addr.Address == "" {
				continue
			}

			// Skip kernel_ll addresses as they'll be automatically re-created
			if strings.Contains(addr.Line, "proto kernel_ll") {
				debugPrint(fmt.Sprintf("Skipping kernel-generated link-local IPv6 address: %s", addr.Address))
				continue
			}

			var ipArgs []string

			// Start with basic arguments
			ipArgs = []string{"-6", "addr", "add", addr.Address, "dev", info.Name}

			// Add scope if present
			if addr.Scope != "" {
				ipArgs = append(ipArgs, "scope", addr.Scope)
			}

			debugPrint(fmt.Sprintf("Running: ip %s", strings.Join(ipArgs, " ")))

			if err := runCommandNoOutput("ip", ipArgs...); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to restore IPv6 address %s to %s: %v\n"+colorReset,
					addr.Address, info.Name, err)
			} else {
				debugPrint(fmt.Sprintf("Restored IPv6 address %s to %s", addr.Address, info.Name))
			}
		}

		// Set interface up if it was up before
		if info.Up {
			if err := runCommandNoOutput("ip", "link", "set", "dev", info.Name, "up"); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to bring interface %s up: %v\n"+colorReset, info.Name, err)
			} else {
				debugPrint(fmt.Sprintf("Brought interface %s up", info.Name))
			}
		}

		// Wait a bit for interface to stabilize
		time.Sleep(1 * time.Second)

		// Verify final state
		finalAddr, _ := runCommand("ip", "addr", "show", "dev", info.Name)
		debugPrint(fmt.Sprintf("Final addresses after restoration:\n%s", finalAddr))
	}

	return nil
}

// reloadIgbDriver unloads and reloads the igb driver while preserving interface configurations
func reloadIgbDriver() error {
	// Identify interfaces using igb driver
	igbInterfaces, err := identifyIgbInterfaces()
	if err != nil {
		return fmt.Errorf("failed to identify igb interfaces: %v", err)
	}

	if len(igbInterfaces) == 0 {
		debugPrint("No interfaces using igb driver found, skipping driver reload")
		return nil
	}

	fmt.Printf(colorGreen+"[INFO] Found %d interfaces using igb driver: %v\n"+colorReset,
		len(igbInterfaces), igbInterfaces)

	// Save current interface configurations
	interfaceConfigs, err := captureInterfaceIPs(igbInterfaces)
	if err != nil {
		return fmt.Errorf("failed to capture interface configurations: %v", err)
	}

	// Unload igb driver
	fmt.Println(colorGreen + "[INFO] Unloading igb driver..." + colorReset)
	if err := runCommandNoOutput("rmmod", "igb"); err != nil {
		return fmt.Errorf("failed to unload igb driver: %v", err)
	}

	// Wait a bit for hardware to settle
	time.Sleep(1 * time.Second)

	// Reload igb driver
	fmt.Println(colorGreen + "[INFO] Reloading igb driver..." + colorReset)
	if err := runCommandNoOutput("modprobe", "igb"); err != nil {
		return fmt.Errorf("failed to reload igb driver: %v", err)
	}

	// Wait for interfaces to be recreated
	fmt.Println(colorGreen + "[INFO] Waiting for interfaces to initialize..." + colorReset)
	time.Sleep(2 * time.Second)

	// Restore interface configurations
	fmt.Println(colorGreen + "[INFO] Restoring interface configurations..." + colorReset)
	if err := restoreInterfaceIPs(interfaceConfigs); err != nil {
		return fmt.Errorf("failed to restore interface configurations: %v", err)
	}

	fmt.Println(colorGreen + "[INFO] Successfully reloaded igb driver and restored configurations" + colorReset)
	return nil
}

// Now modify the writeMAcWithRetries function to include the igb driver reload

func writeMAcWithRetries(macInput string) error {
	targetMAC := strings.ToLower(macInput)
	var allMACs []string // Список всех MAC-адресов для записи в EFI

	// Проверяем метод прошивки
	if macFlashingMethod == "eeupdate" {
		debugPrint("Using eeupdate64e for MAC address flashing on server boards")

		// Обнаруживаем Intel сетевые карты
		cards, err := detectIntelNetworkCards()
		if err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to detect Intel network cards: %v\n"+colorReset, err)
			// Пробуем использовать стандартный метод как запасной вариант
			fmt.Println(colorYellow + "[WARNING] Trying to use standard MAC flashing method" + colorReset)
		} else if len(cards) > 0 {
			// Прошиваем MAC на каждую карту, увеличивая адрес для каждой последующей карты
			currentMAC := targetMAC
			allMACs = append(allMACs, currentMAC) // Добавляем первый MAC в список

			// Статус прошивки для отслеживания общего результата
			flashStatus := true
			for i := range cards {
				nicIndex := i + 1

				// Пропускаем первую карту в этом цикле, так как она уже обработана выше
				if i > 0 {
					// Увеличиваем MAC для следующей карты
					var err error
					currentMAC, err = incrementMAC(currentMAC)
					if err != nil {
						criticalError(fmt.Sprintf("Failed to increment MAC address: %v", err))
						return fmt.Errorf("failed to increment MAC address: %v", err)
					}
					allMACs = append(allMACs, currentMAC) // Добавляем в список
					debugPrint(fmt.Sprintf("MAC address incremented for card #%d: %s", nicIndex, currentMAC))
				}

				debugPrint(fmt.Sprintf("Flashing MAC %s to card #%d", currentMAC, nicIndex))

				// Пробуем прошить MAC с повторами
				var success bool
				for retry := 0; retry < maxRetries; retry++ {
					if err := writeMAcWithEeupdate(currentMAC, nicIndex); err != nil {
						fmt.Printf(colorYellow+"[WARNING] Attempt %d: Failed to flash MAC to card #%d: %v\n"+colorReset, retry+1, nicIndex, err)
						time.Sleep(1 * time.Second)
						continue
					}
					success = true
					break
				}

				if !success {
					fmt.Printf(colorYellow+"[WARNING] Failed to flash MAC to card #%d after %d attempts\n"+colorReset, nicIndex, maxRetries)
					flashStatus = false
					// Продолжаем с другими картами, не останавливаемся при неудаче одной карты
				}
			}

			// Проверяем общий результат прошивки
			if !flashStatus {
				// Если не удалось прошить хотя бы одну карту, предлагаем продолжить со стандартным методом
				fmt.Println(colorYellow + "[WARNING] Some cards failed MAC address flashing with eeupdate" + colorReset)
				// Несмотря на ошибки, продолжаем записывать MAC-адреса в EFI переменные
			}

			// Reload igb driver if present and restore network configuration
			fmt.Println(colorGreen + "[INFO] Checking for igb driver to reload after MAC update..." + colorReset)
			if err := reloadIgbDriver(); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Error reloading igb driver: %v\n"+colorReset, err)
				fmt.Println(colorYellow + "[WARNING] Network interfaces may need manual restart" + colorReset)
			}

			// MAC успешно прошит на все карты
			successMessage("MAC addresses successfully flashed to all detected Intel network cards")
			return nil
		}
		// Если карты не обнаружены, продолжаем с обычным методом
	}

	// Existing code below remains unchanged

	// Если указанный MAC уже присутствует, пропускаем прошивку
	if ifaces, err := getInterfacesWithMAC(targetMAC); err == nil && len(ifaces) > 0 {
		fmt.Printf(colorGreen+"[INFO] MAC address %s already present on interface(s): %s. Skipping flashing.\n"+colorReset,
			targetMAC, strings.Join(ifaces, ", "))

		// Даже если MAC уже установлен, добавляем его в список для EFI переменных
		allMACs = append(allMACs, targetMAC)

		// Ищем другие интерфейсы с последовательными MAC-адресами
		currentMAC := targetMAC
		for i := 1; i < 8; i++ { // Проверяем до 8 возможных последовательных MAC-адресов
			var err error
			currentMAC, err = incrementMAC(currentMAC)
			if err != nil {
				break
			}

			if ifaces, err := getInterfacesWithMAC(currentMAC); err == nil && len(ifaces) > 0 {
				allMACs = append(allMACs, currentMAC)
				debugPrint(fmt.Sprintf("Found interface with MAC %s", currentMAC))
			} else {
				break
			}
		}

		// Записываем все найденные MAC-адреса в EFI переменные
		if len(allMACs) > 0 {
			if err := writeAllMACsToEfiVars(allMACs); err != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to write all MAC addresses to EFI variables: %v\n"+colorReset, err)
			} else {
				debugPrint("Successfully wrote all MAC addresses to EFI variables")
			}
		}

		return nil
	}

	out, err := runCommand("uname", "-m")
	if err != nil {
		return fmt.Errorf("Failed to get machine architecture: %v", err)
	}
	arch := strings.TrimSpace(out)
	rtnicpgSrcDir := getRtnicpgDirectory()
	rtnic := filepath.Join(rtnicpgSrcDir, "rtnicpg-"+arch)

	oldIface, oldIP, err := getActiveInterfaceAndIP()
	if err != nil {
		fmt.Printf(colorYellow+"[WARNING] %v"+colorReset, err)
	} else {
		debugPrint("Old IP address for interface " + oldIface + ": " + oldIP)
	}

	// First attempt to load the driver as is (will check -driver directory first)
	driverErr := loadDriver()

	// If driver loading fails, try recompiling and loading again
	if driverErr != nil {
		fmt.Printf(colorYellow+"[WARNING] Initial driver load failed: %v\nAttempting to recompile driver..."+colorReset+"\n", driverErr)

		// Force recompilation and saving to the -driver directory
		srcDir := getRtnicpgDirectory()
		compiledDriverDir := getCompiledDriverDirectory()

		// Do the compilation in the source directory
		if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
			fmt.Printf("[INFO] Recompiling driver in %s and saving to %s\n", srcDir, compiledDriverDir)

			if err := runCommandNoOutput("make", "-C", srcDir, "clean", "all"); err != nil {
				criticalError("Failed to recompile driver: " + err.Error())
				return err
			}
			fmt.Println(colorGreen + "[INFO] Driver recompilation successful." + colorReset)

			// Try loading the driver again after recompilation (will save it to -driver dir)
			if driverErr = loadDriver(); driverErr != nil {
				criticalError("Failed to load driver even after recompilation: " + driverErr.Error())
				return driverErr
			}
		} else {
			criticalError("Driver source directory does not exist, cannot recompile driver")
			return fmt.Errorf("Driver source directory does not exist, cannot recompile driver")
		}
	}

	if err := os.Chmod(rtnic, 0755); err != nil {
		return fmt.Errorf("Failed to chmod %s: %v", rtnic, err)
	}

	modmac := strings.ReplaceAll(macInput, ":", "")
	fmt.Println(modmac)

	// Try to write MAC with retries
	var macWriteSuccess bool = false
	var macWriteErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		macWriteErr = runCommandNoOutput(rtnic, "/efuse", "/nicmac", "/nodeid", modmac)

		if macWriteErr == nil {
			fmt.Println(colorGreen + "[INFO] MAC address was successfully written, verifying..." + colorReset)
			macWriteSuccess = true
			break
		} else {
			fmt.Printf(colorYellow+"[WARNING] Attempt %d: Failed to write MAC: %v\n"+colorReset, attempt, macWriteErr)

			if attempt == 1 {
				// On first MAC write failure, try to recompile the driver and save to -driver directory
				fmt.Println(colorYellow + "[WARNING] MAC write failed. Attempting to recompile driver and try again..." + colorReset)
				srcDir := getRtnicpgDirectory()
				compiledDriverDir := getCompiledDriverDirectory()

				if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
					fmt.Printf("[INFO] Recompiling driver from %s and saving to %s\n", srcDir, compiledDriverDir)

					// Recompile from source, this will create the driver in the source directory
					if err := runCommandNoOutput("make", "-C", srcDir, "clean", "all"); err != nil {
						fmt.Printf(colorYellow+"[WARNING] Failed to recompile driver: %v\n"+colorReset, err)
					} else {
						fmt.Println(colorGreen + "[INFO] Driver recompilation successful." + colorReset)

						// This will load and also save the driver to the -driver directory
						if err := loadDriver(); err != nil {
							fmt.Printf(colorYellow+"[WARNING] Failed to reload driver after recompilation: %v\n"+colorReset, err)
						}
					}
				}
			}

			time.Sleep(1 * time.Second) // Longer delay for hardware operations
		}
	}

	if !macWriteSuccess {
		criticalError("Failed to write MAC address after " + fmt.Sprintf("%d", maxRetries) + " attempts: " + macWriteErr.Error())
		return fmt.Errorf("Failed to write MAC address after %d attempts: %v", maxRetries, macWriteErr)
	}

	_ = runCommandNoOutput("rmmod", "pgdrv")
	if rtDrv != "" {
		if err := runCommandNoOutput("modprobe", rtDrv); err != nil {
			fmt.Printf(colorYellow+"[WARNING] Failed to modprobe %s: %v\n"+colorReset, rtDrv, err)
		}
	}

	ifaces, err := getInterfacesWithMAC(targetMAC)
	if err != nil {
		return fmt.Errorf("Failed to find interface with target MAC: %v", err)
	}
	debugPrint(fmt.Sprintf("Found interfaces with MAC %s: %v", targetMAC, ifaces))

	var newIface string
	if oldIface != "" {
		for _, iface := range ifaces {
			if iface == oldIface {
				newIface = iface
				break
			}
		}
	}
	if newIface == "" {
		newIface = ifaces[0]
		if len(ifaces) > 1 {
			fmt.Printf(colorYellow+"[WARNING] Multiple interfaces with matching MAC found. Using %s\n"+colorReset, newIface)
		}
	}

	if newIface != "" && oldIP != "" {
		maxRetries := 3
		var assignErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			fmt.Printf("[INFO] Attempt %d: Restarting interface %s with IP %s\n", attempt, newIface, oldIP)

			// Disable the interface
			_ = runCommandNoOutput("ip", "link", "set", "dev", newIface, "down")

			// Remove all IP addresses from the interface
			_ = runCommandNoOutput("ip", "addr", "flush", "dev", newIface)

			// Set MAC address
			_ = runCommandNoOutput("ip", "link", "set", "dev", newIface, "address", targetMAC)

			// Enable the interface
			_ = runCommandNoOutput("ip", "link", "set", "dev", newIface, "up")

			// Check if the interface already has an IP address
			ipCheckOutput, _ := runCommand("ip", "addr", "show", "dev", newIface)

			// If there's any IP already assigned, try to remove it specifically
			if strings.Contains(ipCheckOutput, "inet ") {
				debugPrint("Interface still has IP addresses after flush, attempting to remove them individually")

				// Parse and remove any existing IP addresses
				ipRegex := regexp.MustCompile(`inet\s+([0-9.]+/[0-9]+)`)
				matches := ipRegex.FindAllStringSubmatch(ipCheckOutput, -1)

				for _, match := range matches {
					if len(match) >= 2 {
						existingIP := match[1]
						debugPrint(fmt.Sprintf("Removing IP: %s", existingIP))
						_ = runCommandNoOutput("ip", "addr", "del", existingIP, "dev", newIface)
					}
				}
			}

			// Add original IP
			assignErr = runCommandNoOutput("ip", "addr", "add", oldIP, "dev", newIface)

			if assignErr == nil {
				fmt.Printf(colorGreen+"[INFO] Interface %s restarted with IP %s\n"+colorReset, newIface, oldIP)
				break
			} else {
				fmt.Printf(colorYellow+"[WARNING] Attempt %d: Failed to assign IP %s to interface %s: %v\n"+colorReset, attempt, oldIP, newIface, assignErr)

				// Check if the IP is already assigned and is the correct one
				ipCheckOutput, _ := runCommand("ip", "addr", "show", "dev", newIface)
				if strings.Contains(ipCheckOutput, oldIP) {
					fmt.Printf(colorGreen+"[INFO] IP %s is already assigned to %s, continuing...\n"+colorReset, oldIP, newIface)
					assignErr = nil
					break
				}

				// Check if there are new interfaces with the target MAC
				if newIfaces, err := getInterfacesWithMAC(targetMAC); err == nil && len(newIfaces) > 0 {
					// Check if interface name has changed
					foundDifferent := false
					for _, iface := range newIfaces {
						if iface != newIface {
							newIface = iface
							foundDifferent = true
							fmt.Printf("[INFO] Retrying with interface %s\n", newIface)
							break
						}
					}
					// If no new interface found, continue with current one
					if !foundDifferent {
						fmt.Printf("[INFO] Still using interface %s\n", newIface)
					}
				} else {
					fmt.Println(colorYellow + "[WARNING] No interface with target MAC found on retry" + colorReset)
				}
			}

			if attempt == maxRetries && assignErr != nil {
				fmt.Printf(colorYellow+"[WARNING] Failed to assign IP after %d attempts: %v. Network configuration may need manual adjustment.\n"+colorReset, maxRetries, assignErr)
			}
		}
	} else {
		fmt.Println(colorYellow + "[WARNING] Could not find interface for " + targetMAC + " or no previous IP was stored." + colorReset)
	}

	if len(allMACs) == 0 {
		allMACs = append(allMACs, targetMAC)
	}

	if err := writeAllMACsToEfiVars(allMACs); err != nil {
		fmt.Printf(colorYellow+"[WARNING] Failed to write all MAC addresses to EFI variables: %v\n"+colorReset, err)
	} else {
		debugPrint("Successfully wrote all MAC addresses to EFI variables")
	}

	return nil
}
