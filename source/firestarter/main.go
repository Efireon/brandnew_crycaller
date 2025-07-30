package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0x5a17ed/uefi/efi/efiguid"
	"github.com/0x5a17ed/uefi/efi/efivario"
	"gopkg.in/yaml.v3"
)

const VERSION = "2.1.2"

// ANSI color codes
const (
	// Существующие константы остаются
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorGray   = "\033[90m" // Добавить если нет
	ColorCyan   = "\033[36m" // Добавить если нет

	// НОВЫЕ константы для фонов:
	ColorBgGreen  = "\033[42m\033[30m" // Зеленый фон, черный текст
	ColorBgRed    = "\033[41m\033[37m" // Красный фон, белый текст
	ColorBgYellow = "\033[43m\033[30m" // Желтый фон, черный текст
	ColorBgBlue   = "\033[44m\033[37m" // Синий фон, белый текст
)

// Configuration structures
type Config struct {
	System SystemConfig `yaml:"system"`
	Tests  TestsConfig  `yaml:"tests"`
	Flash  FlashConfig  `yaml:"flash,omitempty"`
	Log    LogConfig    `yaml:"log"`
}

type SystemConfig struct {
	Product      string `yaml:"product"`
	Manufacturer string `yaml:"manufacturer"`
	RequireRoot  bool   `yaml:"require_root"`
	GuidPrefix   string `yaml:"guid_prefix"`
	EfiSnName    string `yaml:"efi_sn_name"`
	EfiMacName   string `yaml:"efi_mac_name"`
	DriverDir    string `yaml:"driver_dir"`
}

type TestsConfig struct {
	Timeout          string       `yaml:"timeout,omitempty"`
	ParallelGroups   [][]TestSpec `yaml:"parallel_groups,omitempty"`
	SequentialGroups [][]TestSpec `yaml:"sequential_groups,omitempty"`
}

type TestSpec struct {
	Name     string   `yaml:"name"`
	Command  string   `yaml:"command"`
	Args     []string `yaml:"args,omitempty"`
	Type     string   `yaml:"type"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Required bool     `yaml:"required"`
	Collapse bool     `yaml:"collapse,omitempty"` // Новое поле: если true — при успехе не показываем вывод
}

type FlashField struct {
	Name  string `yaml:"name"`
	Flash bool   `yaml:"flash"`
	ID    string `yaml:"id"`
	Regex string `yaml:"regex"`
}

type FlashConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Operations []string     `yaml:"operations,omitempty"`
	Fields     []FlashField `yaml:"fields,omitempty"`
	Method     string       `yaml:"method,omitempty"`
	VenDevice  []string     `yaml:"ven_device,omitempty"`
}

type FRUStatus struct {
	IsPresent    bool
	IsEmpty      bool
	HasBadSum    bool
	CanRead      bool
	ErrorMessage string
}

type LogConfig struct {
	SaveLocal bool   `yaml:"save_local"`
	SendLogs  bool   `yaml:"send_logs"`
	LogDir    string `yaml:"log_dir,omitempty"`
	Server    string `yaml:"server,omitempty"`
	ServerDir string `yaml:"server_dir,omitempty"`
	OpName    string `yaml:"op_name,omitempty"`
}

type FlashData struct {
	SystemSerial string
	IOBoard      string
	MAC          string
}

// Result structures
type TestResult struct {
	Name     string        `yaml:"name"`
	Status   string        `yaml:"status"` // "PASSED", "FAILED", "TIMEOUT", "SKIPPED"
	Duration time.Duration `yaml:"duration"`
	Error    string        `yaml:"error,omitempty"`
	Output   string        `yaml:"-"` // Not saved to log
	Required bool          `yaml:"required"`
	Attempts int           `yaml:"attempts,omitempty"`
}

type SystemInfo struct {
	Product   string    `yaml:"product"`
	MBSerial  string    `yaml:"mb_serial,omitempty"` // Прошитый серийник материнской платы
	IOSerial  string    `yaml:"io_serial,omitempty"` // Прошитый серийник IO платы
	MAC       string    `yaml:"mac,omitempty"`       // Прошитый MAC адрес
	IP        string    `yaml:"ip,omitempty"`
	Timestamp time.Time `yaml:"timestamp"`

	// Оригинальные значения (до прошивки)
	OriginalMBSerial string   `yaml:"original_mb_serial,omitempty"` // Оригинальный серийник материнской платы
	OriginalMACs     []string `yaml:"original_macs,omitempty"`      // Список всех оригинальных MAC адресов

	// DMIDecode данные в конце для лучшей читаемости
	DMIDecode map[string]interface{} `yaml:"dmidecode"`
}

// Обновленная структура SessionLog - тесты перенесены ближе к началу
type SessionLog struct {
	SessionID    string        `yaml:"session"`
	Timestamp    time.Time     `yaml:"timestamp"`
	State        string        `yaml:"state"`
	Pipeline     PipelineInfo  `yaml:"pipeline"`
	TestResults  []TestResult  `yaml:"test_results"`
	FlashResults []FlashResult `yaml:"flash_results,omitempty"`
	System       SystemInfo    `yaml:"system"`
}

type PipelineInfo struct {
	Mode     string        `yaml:"mode"`
	Config   string        `yaml:"config"`
	Duration time.Duration `yaml:"duration"`
	Operator string        `yaml:"operator"`
}

type FlashResult struct {
	Operation string        `yaml:"operation"`
	Status    string        `yaml:"status"`
	Duration  time.Duration `yaml:"duration"`
	Details   string        `yaml:"details,omitempty"`
}

// Network interface management
type NetworkInterface struct {
	Name   string
	MAC    string
	IP     string
	Driver string
	State  string
}

type IntelNIC struct {
	Index        int
	VendorDevice string
	Description  string
}

type FlashMACSummary struct {
	Method         string
	TargetMAC      string
	ExistingMAC    bool
	InterfaceName  string
	OriginalIP     string
	OriginalDriver string
	NICIndices     []int // For eeupdate method
	Success        bool
	Error          string
}

// Output manager for synchronized output
type OutputManager struct {
	mutex sync.Mutex
}

// Структура для резервной копии сетевого состояния
type NetworkBackup struct {
	Timestamp     time.Time
	Interfaces    []NetworkInterface
	LoadedModules []string
}

// getTerminalWidth получает ширину терминала
func getTerminalWidth() int {
	// Попробуем получить через stty
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if output, err := cmd.Output(); err == nil {
		parts := strings.Fields(string(output))
		if len(parts) >= 2 {
			if w, err := strconv.Atoi(parts[1]); err == nil && w > 0 {
				return w
			}
		}
	}

	// Fallback на переменную окружения
	if width := os.Getenv("COLUMNS"); width != "" {
		if w, err := strconv.Atoi(width); err == nil && w > 0 {
			return w
		}
	}

	// Значение по умолчанию
	return 80
}

// printSeparator печатает горизонтальную линию по ширине терминала
func printSeparator() {
	width := getTerminalWidth()
	fmt.Printf("%s%s%s\n", ColorGray, strings.Repeat("─", width), ColorReset)
}

// printThickSeparator печатает толстую горизонтальную линию
func printThickSeparator() {
	width := getTerminalWidth()
	fmt.Printf("%s%s%s\n", ColorGray, strings.Repeat("═", width), ColorReset)
}

func (om *OutputManager) PrintSection(title, content string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
	printSeparator()

	// Выводим контент как есть
	fmt.Print(content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Println()
	}

	// Пустая строка после контента для отделения от результата
	fmt.Println()
}

func (om *OutputManager) PrintResult(timestamp time.Time, name, status string, duration time.Duration, err string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	// Форматируем статус в enterprise стиле
	var statusBlock string
	switch status {
	case "PASSED":
		statusBlock = fmt.Sprintf("%s PASSED %s", ColorBgGreen, ColorReset)
	case "FAILED":
		statusBlock = fmt.Sprintf("%s FAILED %s", ColorBgRed, ColorReset)
	case "TIMEOUT":
		statusBlock = fmt.Sprintf("%s TIMEOUT %s", ColorBgYellow, ColorReset)
	case "SKIPPED":
		statusBlock = fmt.Sprintf("%s SKIPPED %s", ColorBgYellow, ColorReset)
	case "RUNNING":
		statusBlock = fmt.Sprintf("%s RUNNING %s", ColorBgBlue, ColorReset)
	default:
		statusBlock = fmt.Sprintf("%s UNKNOWN %s", ColorWhite, ColorReset)
	}

	// Основная строка результата
	fmt.Printf("%s[%s]%s %s | Duration: %s%s%s",
		ColorGray, timestamp.Format("15:04:05"), ColorReset,
		statusBlock,
		ColorGray, duration.Round(100*time.Millisecond), ColorReset)

	// Добавляем код ошибки если есть
	if err != "" && status != "RUNNING" {
		// Пытаемся извлечь exit code из ошибки
		if strings.Contains(err, "Exit code:") {
			fmt.Printf(" | Exit Code: %s%s%s", ColorRed, strings.TrimPrefix(err, "Exit code: "), ColorReset)
		} else {
			fmt.Printf(" | %sERROR: %s%s", ColorRed, err, ColorReset)
		}
	}

	fmt.Println()
}

func printTestsSummary(results []TestResult, duration time.Duration) {
	// Заголовок
	fmt.Printf("\n%sTESTS SUMMARY%s\n", ColorWhite, ColorReset)
	printThickSeparator()

	// Подсчёт статусов
	total := len(results)
	passed, failed, skipped, timedOut := 0, 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "PASSED":
			passed++
		case "FAILED":
			failed++
		case "SKIPPED":
			skipped++
		case "TIMEOUT":
			timedOut++
		}
	}

	// Отображение метрик
	fmt.Printf("  %-15s: %s%4d%s\n", "Total Tests", ColorWhite, total, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Passed", ColorGreen, passed, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Failed", ColorRed, failed, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Skipped", ColorYellow, skipped, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Timed Out", ColorYellow, timedOut, ColorReset)

	// Процент успешных
	if total > 0 {
		rate := (passed * 100) / total
		rateColor := ColorRed
		switch {
		case rate == 100:
			rateColor = ColorGreen
		case rate >= 80:
			rateColor = ColorYellow
		}
		fmt.Printf("  %-15s: %s%3d%%%s\n", "Success Rate", rateColor, rate, ColorReset)
	}

	// Время выполнения
	fmt.Printf("  %-15s: %s%v%s\n", "Elapsed Time", ColorGray, duration.Round(time.Second), ColorReset)

	// Разделитель перед списком
	printThickSeparator()

	// Список тестов, которые не прошли
	if failed+timedOut > 0 {
		fmt.Printf("\n%sNOT PASSED TESTS (%d)%s\n", ColorRed, failed+timedOut, ColorReset)
		for _, r := range results {
			if r.Status == "FAILED" || r.Status == "TIMEOUT" {
				fmt.Printf("  - %s%s%s\n", ColorRed, r.Name, ColorReset)
			}
		}
	} else {
		fmt.Printf("\n%sALL TESTS PASSED%s\n", ColorGreen, ColorReset)
	}

	fmt.Println()
}

var outputManager = &OutputManager{}

func printSectionHeader(title string) {
	fmt.Printf("\n%s%s%s Hardware Validation System %sv%s%s\n",
		ColorBlue, "FIRESTARTER", ColorReset, ColorGray, VERSION, ColorReset)
	printThickSeparator()
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
}

func printSubHeader(title, subtitle string) {
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
	if subtitle != "" {
		fmt.Printf("%s%s%s\n", ColorGray, subtitle, ColorReset)
	}
}

// printExecutionSummary выводит сводку по сессии и затем детальный вывод всех упавших тестов
func printExecutionSummary(allResults []TestResult, flashResults []FlashResult, totalDuration time.Duration) {
	fmt.Printf("\n%sSESSION SUMMARY%s\n", ColorWhite, ColorReset)
	printThickSeparator()

	// Собираем статистику тестов
	totalTests := len(allResults)
	passedTests := 0
	failedTests := 0
	skippedTests := 0
	timeoutTests := 0

	for _, result := range allResults {
		switch result.Status {
		case "PASSED":
			passedTests++
		case "FAILED":
			failedTests++
		case "SKIPPED":
			skippedTests++
		case "TIMEOUT":
			timeoutTests++
		}
	}

	// Собираем статистику прошивки
	totalFlash := len(flashResults)
	successFlash := 0
	failedFlash := 0
	for _, fr := range flashResults {
		if fr.Status == "SUCCESS" || fr.Status == "COMPLETED" || fr.Status == "PASSED" {
			successFlash++
		} else {
			failedFlash++
		}
	}

	// Выводим основные цифры
	fmt.Printf("  Total Tests       : %s%d%s\n", ColorWhite, totalTests, ColorReset)
	fmt.Printf("  Passed            : %s%d%s\n", ColorGreen, passedTests, ColorReset)
	fmt.Printf("  Failed            : %s%d%s\n", ColorRed, failedTests, ColorReset)
	fmt.Printf("  Skipped           : %s%d%s\n", ColorYellow, skippedTests, ColorReset)
	fmt.Printf("  Timeout           : %s%d%s\n", ColorYellow, timeoutTests, ColorReset)
	if totalTests > 0 {
		successRate := (passedTests * 100) / totalTests
		color := ColorRed
		if successRate >= 100 {
			color = ColorGreen
		} else if successRate >= 80 {
			color = ColorYellow
		}
		fmt.Printf("  Success Rate      : %s%d%%%s\n", color, successRate, ColorReset)
	}

	if totalFlash > 0 {
		fmt.Printf("\n  Flash Operations  : %s%d Total%s\n", ColorWhite, totalFlash, ColorReset)
		fmt.Printf("  Flash Success     : %s%d%s\n", ColorGreen, successFlash, ColorReset)
		fmt.Printf("  Flash Failed      : %s%d%s\n", ColorRed, failedFlash, ColorReset)
	}

	fmt.Printf("\n  Total Duration    : %s%s%s\n", ColorGray, totalDuration.Round(time.Second), ColorReset)

	// Определяем и выводим общий статус
	sessionStatus := "SUCCESS"
	if failedTests > 0 || failedFlash > 0 {
		sessionStatus = "FAILED"
	} else if skippedTests > 0 || timeoutTests > 0 {
		sessionStatus = "PARTIAL"
	}
	fmt.Printf("  Session Status    : ")
	switch sessionStatus {
	case "SUCCESS":
		fmt.Printf("%s SUCCESS %s\n", ColorBgGreen, ColorReset)
	case "FAILED":
		fmt.Printf("%s FAILED %s %s(issues detected)%s\n", ColorBgRed, ColorReset, ColorGray, ColorReset)
	case "PARTIAL":
		fmt.Printf("%s PARTIAL %s %s(some tests skipped)%s\n", ColorBgYellow, ColorReset, ColorGray, ColorReset)
	}

	// Если есть упавшие тесты — показываем их список
	if failedTests > 0 {
		fmt.Printf("\n%sCRITICAL ISSUES REQUIRING ATTENTION%s\n", ColorWhite, ColorReset)
		printSeparator()
		for _, result := range allResults {
			if result.Status == "FAILED" || result.Status == "TIMEOUT" {
				fmt.Printf("  %s%-20s%s %s\n", ColorRed, result.Name, ColorReset,
					func() string {
						if result.Error != "" {
							return result.Error
						}
						return "Test execution failed"
					}())
			}
		}
	}
}

func printColored(color, message string) {
	fmt.Printf("%s%s%s\n", color, message, ColorReset)
}

func printInfo(message string) {
	printColored(ColorBlue, message)
}

func printDebug(message string) {
	printColored(ColorWhite, message)
}

func printSuccess(message string) {
	printColored(ColorGreen, message)
}

func printWarning(message string) {
	printColored(ColorYellow, message)
}

func printError(message string) {
	printColored(ColorRed, message)
}

func showHelp() {
	fmt.Printf("System Validator %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file (default: config.yaml)")
	fmt.Println("  -tests-only Run only tests (skip flashing)")
	fmt.Println("  -flash-only Run only flashing (skip tests)")
	fmt.Println("  -h          Show this help")
}

func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
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

func askUserAction(testName string) string {
	fmt.Printf("\n%s=== TEST FAILED ===%s\n", ColorRed, ColorReset)
	fmt.Printf("Test '%s' has failed.\n", testName)
	fmt.Printf("Choose action:\n")
	fmt.Printf("  %s[Y]%s Yes - Retry test (default)\n", ColorGreen, ColorReset)
	fmt.Printf("  %s[N]%s No  - Continue with next test\n", ColorYellow, ColorReset)
	fmt.Printf("  %s[S]%s Skip - Mark as skipped by operator\n", ColorBlue, ColorReset)
	fmt.Printf("Choice [Y/n/s]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "Y" // Default on error
	}

	choice := strings.ToUpper(strings.TrimSpace(input))
	if choice == "" {
		choice = "Y" // Default
	}

	switch choice {
	case "Y", "YES":
		return "RETRY"
	case "N", "NO":
		return "CONTINUE"
	case "S", "SKIP":
		return "SKIP"
	default:
		fmt.Printf("Invalid choice '%s', defaulting to retry.\n", choice)
		return "RETRY"
	}
}

func askUserProductMismatch(configProduct, detectedProduct string) bool {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\n%s⚠️  PRODUCT MISMATCH WARNING ⚠️%s\n", ColorRed, ColorReset)
	fmt.Printf("Configuration file is designed for: %s%s%s\n", ColorYellow, configProduct, ColorReset)
	fmt.Printf("Detected system product: %s%s%s\n", ColorYellow, detectedProduct, ColorReset)
	fmt.Printf("\nThis configuration may not be suitable for your hardware.\n")
	fmt.Printf("Continuing may lead to unexpected behavior or hardware damage.\n\n")

	for {
		fmt.Printf("Do you want to close the program? %s[Y/n]%s: ", ColorGreen, ColorReset)

		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("%sError reading input: %v%s\n", ColorRed, err, ColorReset)
			continue
		}

		input = strings.TrimSpace(strings.ToLower(input))

		// Default is 'Y' (close program)
		if input == "" || input == "y" || input == "yes" {
			return true // Close program
		} else if input == "n" || input == "no" {
			return false // Continue
		} else {
			fmt.Printf("%sPlease enter 'Y' to close or 'N' to continue.%s\n", ColorRed, ColorReset)
		}
	}
}

func executeTest(test TestSpec, globalTimeout string) (TestResult, string) {
	result := TestResult{
		Name:     test.Name,
		Status:   "FAILED",
		Required: test.Required,
	}

	startTime := time.Now()

	// Parse timeout - приоритет: тест > глобальный > дефолт
	timeout := 30 * time.Second
	if test.Timeout != "" {
		if t, err := time.ParseDuration(test.Timeout); err == nil {
			timeout = t
		}
	} else if globalTimeout != "" {
		if t, err := time.ParseDuration(globalTimeout); err == nil {
			timeout = t
		}
	}

	// Create command
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, test.Command, test.Args...)

	// Capture both stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run command
	err := cmd.Run()
	result.Duration = time.Since(startTime)

	// Combine output for display
	output := stdout.String() + stderr.String()
	result.Output = output

	// Determine result
	if ctx.Err() == context.DeadlineExceeded {
		result.Status = "TIMEOUT"
		result.Error = fmt.Sprintf("Test timed out after %s", timeout)
	} else if err != nil {
		result.Status = "FAILED"
		// Try to get error message from stderr
		if stderr.Len() > 0 {
			lines := strings.Split(stderr.String(), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "ERROR:") {
					result.Error = strings.TrimPrefix(line, "ERROR:")
					result.Error = strings.TrimSpace(result.Error)
					break
				}
			}
		}
		if result.Error == "" {
			result.Error = fmt.Sprintf("Exit code: %d", cmd.ProcessState.ExitCode())
		}
	} else {
		result.Status = "PASSED"
	}

	return result, output
}

// runTest выполняет тест и возвращает результат, не выводя сразу секцию с полным выводом
func runTest(test TestSpec, outputMgr *OutputManager, globalTimeout string) TestResult {
	attempts := 0
	maxAttempts := 5

	var result TestResult
	var output string

	for attempts < maxAttempts {
		attempts++
		outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")

		result, output = executeTest(test, globalTimeout)
		result.Attempts = attempts
		result.Output = output

		outputMgr.PrintResult(time.Now(), test.Name, result.Status, result.Duration, result.Error)

		// Решаем, показывать ли полный вывод:
		if output != "" && !(result.Status == "PASSED" && test.Collapse) {
			outputMgr.PrintSection(test.Name+" Output", output)
		}

		if result.Status == "PASSED" {
			return result
		}

		action := askUserAction(test.Name)
		switch action {
		case "RETRY":
			// Показываем вывод предыдущего неудачного теста перед повтором
			if result.Output != "" {
				fmt.Printf("%sPrevious test output:%s\n", ColorYellow, ColorReset)
				outputMgr.PrintSection(test.Name+" Previous Output", result.Output)
			}

			fmt.Printf("%sRetrying test '%s' (attempt %d)...%s\n\n", ColorBlue, test.Name, attempts+1, ColorReset)
			continue
		case "SKIP":
			result.Status = "SKIPPED"
			result.Error = "Skipped by operator"
			return result
		case "CONTINUE":
			return result
		}
	}

	// Если дошли до лимита попыток
	fmt.Printf("%sMaximum retry attempts (%d) reached for test '%s'%s\n", ColorRed, maxAttempts, test.Name, ColorReset)
	finalResult, finalOutput := executeTest(test, globalTimeout)
	finalResult.Attempts = attempts
	finalResult.Output = finalOutput

	outputMgr.PrintResult(time.Now(), test.Name, finalResult.Status, finalResult.Duration, finalResult.Error)
	if finalOutput != "" && !(finalResult.Status == "PASSED" && test.Collapse) {
		outputMgr.PrintSection(test.Name+" Output", finalOutput)
	}
	return finalResult
}

// runParallelTestsWithRetries выполняет набор тестов параллельно, а потом последовательно обрабатывает упавшие,
// показывая при этом сразу причину и вывод для каждого неудачного теста.
func runParallelTestsWithRetries(tests []TestSpec, outputMgr *OutputManager, globalTimeout string) []TestResult {
	results := make([]TestResult, len(tests))
	finalResults := make([]TestResult, len(tests))

	// --- Параллельный запуск ---
	var wg sync.WaitGroup
	for i, t := range tests {
		wg.Add(1)
		go func(idx int, test TestSpec) {
			defer wg.Done()

			outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")
			res, out := executeTest(test, globalTimeout)
			res.Attempts = 1
			res.Output = out

			outputMgr.PrintResult(time.Now(), test.Name, res.Status, res.Duration, res.Error)
			if out != "" && !(res.Status == "PASSED" && test.Collapse) {
				outputMgr.PrintSection(test.Name+" Output", out)
			}

			results[idx] = res
		}(i, t)
	}
	wg.Wait()

	// --- Подсчитываем упавшие ---
	failedCount := 0
	for _, r := range results {
		if r.Status == "FAILED" || r.Status == "TIMEOUT" {
			failedCount++
		}
	}
	if failedCount > 0 {
		fmt.Printf("\n%sParallel complete: %d failed test(s)%s\n", ColorYellow, failedCount, ColorReset)
	} else {
		fmt.Printf("\n%sAll parallel tests passed%s\n", ColorGreen, ColorReset)
	}

	// --- Последовательная доработка упавших ---
	proc := 0
	for i, r := range results {
		if r.Status == "PASSED" {
			finalResults[i] = r
			continue
		}
		proc++
		if proc > 1 {
			fmt.Println()
		}
		fmt.Printf("%sProcessing failed test %d/%d: %s%s\n",
			ColorBlue, proc, failedCount, tests[i].Name, ColorReset)

		// Всегда показываем причину и вывод перед retry/skip
		fmt.Printf("  Status: %s%s%s\n", ColorRed, r.Status, ColorReset)
		if r.Error != "" {
			fmt.Printf("  Error : %s\n", r.Error)
		}
		if r.Output != "" {
			outputMgr.PrintSection(tests[i].Name+" Output", r.Output)
		}

		finalResults[i] = handleFailedTestWithRetries(tests[i], r, outputMgr, globalTimeout)
	}

	return finalResults
}

// handleFailedTestWithRetries предлагает retry/skip/continue до 5 раз
func handleFailedTestWithRetries(test TestSpec, initialResult TestResult, outputMgr *OutputManager, globalTimeout string) TestResult {
	currentResult := initialResult
	attempts := initialResult.Attempts
	maxAttempts := 5

	for attempts < maxAttempts && currentResult.Status != "PASSED" {
		action := askUserAction(test.Name)
		switch action {
		case "RETRY":
			attempts++

			// Показываем вывод предыдущего неудачного теста перед повтором
			if currentResult.Output != "" {
				fmt.Printf("%sPrevious test output:%s\n", ColorYellow, ColorReset)
				outputMgr.PrintSection(test.Name+" Previous Output", currentResult.Output)
			}

			fmt.Printf("%sRetrying test '%s' (attempt %d)...%s\n\n", ColorBlue, test.Name, attempts, ColorReset)
			outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")
			result, output := executeTest(test, globalTimeout)
			result.Attempts = attempts
			result.Output = output
			outputMgr.PrintResult(time.Now(), test.Name, result.Status, result.Duration, result.Error)
			currentResult = result
		case "SKIP":
			currentResult.Status = "SKIPPED"
			currentResult.Error = "Skipped by operator"
			outputMgr.PrintResult(time.Now(), test.Name, currentResult.Status, currentResult.Duration, currentResult.Error)
			return currentResult
		case "CONTINUE":
			return currentResult
		}
	}

	if attempts >= maxAttempts && currentResult.Status != "PASSED" {
		fmt.Printf("%sMaximum retry attempts (%d) reached for test '%s'%s\n", ColorRed, maxAttempts, test.Name, ColorReset)
	}

	return currentResult
}

func runTestGroup(tests []TestSpec, parallel bool, outputMgr *OutputManager, groupName, globalTimeout string) []TestResult {
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(groupName), ColorReset)

	mode := "Sequential"
	if parallel {
		mode = "Parallel"
	}

	fmt.Printf("Mode: %s%s%s | Tests: %s%d%s | Timeout: %s%s%s\n",
		ColorCyan, mode, ColorReset,
		ColorGreen, len(tests), ColorReset,
		ColorYellow, func() string {
			if globalTimeout != "" {
				return globalTimeout
			}
			return "30s (default)"
		}(), ColorReset)

	printSeparator()

	var results []TestResult
	if parallel {
		results = runParallelTestsWithRetries(tests, outputMgr, globalTimeout)
	} else {
		results = make([]TestResult, len(tests))
		for i, test := range tests {
			results[i] = runTest(test, outputMgr, globalTimeout)
		}
	}

	// Выводим сводку группы в enterprise стиле
	fmt.Printf("\n%sGROUP RESULTS%s\n", ColorWhite, ColorReset)
	printSeparator()

	passed := 0
	failed := 0
	skipped := 0

	var passedTests []string
	var failedTests []string
	var skippedTests []string

	for _, result := range results {
		switch result.Status {
		case "PASSED":
			passed++
			passedTests = append(passedTests, result.Name)
		case "FAILED", "TIMEOUT":
			failed++
			failedTests = append(failedTests, result.Name)
		case "SKIPPED":
			skipped++
			skippedTests = append(skippedTests, result.Name)
		}
	}

	// Определяем статус группы
	groupStatus := "PASSED"
	if failed > 0 {
		groupStatus = "FAILED"
	} else if skipped > 0 {
		groupStatus = "PARTIAL"
	}

	// Выводим статистику
	fmt.Printf("  %s%-20s%s: ", ColorWhite, groupName, ColorReset)
	switch groupStatus {
	case "PASSED":
		fmt.Printf("%s PASSED %s", ColorBgGreen, ColorReset)
	case "FAILED":
		fmt.Printf("%s FAILED %s %s(%d of %d tests failed)%s",
			ColorBgRed, ColorReset, ColorGray, failed, len(tests), ColorReset)
	case "PARTIAL":
		fmt.Printf("%s PARTIAL %s %s(%d passed, %d skipped)%s",
			ColorBgYellow, ColorReset, ColorGray, passed, skipped, ColorReset)
	}
	fmt.Println()

	// Выводим списки тестов
	if len(passedTests) > 0 {
		fmt.Printf("  %sPassed:%s %s\n", ColorGreen, ColorReset, strings.Join(passedTests, ", "))
	}
	if len(failedTests) > 0 {
		fmt.Printf("  %sFailed:%s %s\n", ColorRed, ColorReset, strings.Join(failedTests, ", "))
	}
	if len(skippedTests) > 0 {
		fmt.Printf("  %sSkipped:%s %s\n", ColorYellow, ColorReset, strings.Join(skippedTests, ", "))
	}

	return results
}

func getFlashData(config FlashConfig, productName string) (*FlashData, error) {
	if !config.Enabled || len(config.Fields) == 0 {
		return nil, nil
	}

	if productName == "" {
		return nil, fmt.Errorf("product name not detected")
	}

	printSectionHeader("FLASH DATA COLLECTION")
	fmt.Printf("Product: %s%s%s\n", ColorGreen, productName, ColorReset)
	fmt.Printf("Method: %s%s%s\n", ColorGreen, config.Method, ColorReset)
	if len(config.VenDevice) > 0 {
		fmt.Printf("Target Devices: %s%s%s\n", ColorYellow, strings.Join(config.VenDevice, ", "), ColorReset)
	}

	// Prepare fields that need flashing
	requiredFields := make(map[string]*FlashField)
	flashFields := make(map[string]*FlashField)

	fmt.Printf("\nRequired fields:\n")
	for i := range config.Fields {
		field := &config.Fields[i]
		_, err := regexp.Compile(field.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex for field %s: %v", field.Name, err)
		}

		requiredFields[field.ID] = field
		if field.Flash {
			flashFields[field.ID] = field
			fmt.Printf("  %s[FLASH]%s %s (format: %s)\n", ColorYellow, ColorReset, field.Name, field.Regex)
		} else {
			fmt.Printf("  %s[STORE]%s %s (format: %s)\n", ColorBlue, ColorReset, field.Name, field.Regex)
		}
	}

	provided := make(map[string]string)
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\nEnter values (program will auto-detect field type):\n")

	for len(provided) < len(requiredFields) {
		fmt.Printf("\nRemaining fields: %d\n", len(requiredFields)-len(provided))
		fmt.Printf("Enter value: ")

		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		input = strings.TrimSpace(input)

		if input == "" {
			fmt.Printf("%sInput cannot be empty. Please re-enter.%s\n", ColorRed, ColorReset)
			continue
		}

		matched := false
		for fieldID, field := range requiredFields {
			if _, ok := provided[fieldID]; ok {
				continue
			}

			regex, _ := regexp.Compile(field.Regex) // Already validated above
			if regex.MatchString(input) {
				provided[fieldID] = input
				flashStatus := ""
				if field.Flash {
					flashStatus = fmt.Sprintf(" %s[WILL FLASH]%s", ColorYellow, ColorReset)
				} else {
					flashStatus = fmt.Sprintf(" %s[STORED ONLY]%s", ColorBlue, ColorReset)
				}
				fmt.Printf("%s%s accepted: %s%s%s\n", ColorGreen, field.Name, input, flashStatus, ColorReset)
				matched = true
				break
			}
		}

		if !matched {
			fmt.Printf("%sInput does not match any expected format. Please try again.%s\n", ColorRed, ColorReset)
		}
	}

	flashData := &FlashData{}

	// Map fields to FlashData structure
	for fieldID, value := range provided {
		switch fieldID {
		case "system-serial-number":
			flashData.SystemSerial = value
		case "io_board":
			flashData.IOBoard = value
		case "mac_address":
			flashData.MAC = value
		}
	}

	fmt.Printf("\n%sCollected data summary:%s\n", ColorGreen, ColorReset)
	if flashData.SystemSerial != "" {
		fmt.Printf("  System Serial: %s\n", flashData.SystemSerial)
	}
	if flashData.IOBoard != "" {
		fmt.Printf("  IO Board: %s\n", flashData.IOBoard)
	}
	if flashData.MAC != "" {
		fmt.Printf("  MAC Address: %s\n", flashData.MAC)
	}

	return flashData, nil
}

func getSystemInfo() (SystemInfo, error) {
	info := SystemInfo{
		Timestamp: time.Now(),
	}

	// Get IP address
	if ip, err := getIPAddress(); err == nil {
		info.IP = ip
	}

	// Get original MAC addresses from all network interfaces
	if interfaces, err := getCurrentNetworkInterfaces(); err == nil {
		var originalMACs []string
		for _, iface := range interfaces {
			if iface.MAC != "" && iface.Name != "lo" { // Исключаем loopback
				// Нормализуем MAC для единообразия
				normalizedMAC := normalizeMAC(iface.MAC)
				if normalizedMAC != "" {
					originalMACs = append(originalMACs, normalizedMAC)
				}
			}
		}
		info.OriginalMACs = originalMACs

		if len(originalMACs) > 0 {
			printInfo(fmt.Sprintf("Collected %d original MAC address(es): %s",
				len(originalMACs), strings.Join(originalMACs, ", ")))
		}
	} else {
		printWarning(fmt.Sprintf("Failed to collect original MAC addresses: %v", err))
	}

	// Run dmidecode
	cmd := exec.Command("dmidecode")
	output, err := cmd.Output()
	if err != nil {
		return info, fmt.Errorf("failed to run dmidecode: %v", err)
	}

	// Parse dmidecode output
	dmidecodeData := parseDMIDecode(string(output))
	info.DMIDecode = dmidecodeData

	// Extract key information and save original values
	if systemInfo, ok := dmidecodeData["System Information"].(map[string]interface{}); ok {
		if product, ok := systemInfo["Product Name"].(string); ok {
			info.Product = product
		}
	}

	if baseboardInfo, ok := dmidecodeData["Base Board Information"].(map[string]interface{}); ok {
		if serial, ok := baseboardInfo["Serial Number"].(string); ok {
			info.OriginalMBSerial = serial // Сохраняем оригинальный серийник
			printInfo(fmt.Sprintf("Original motherboard serial: %s", serial))
		}
	}

	return info, nil
}

func getIPAddress() (string, error) {
	cmd := exec.Command("hostname", "-I")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	ips := strings.Fields(string(output))
	if len(ips) > 0 {
		return ips[0], nil
	}

	return "", fmt.Errorf("no IP address found")
}

func parseDMIDecode(output string) map[string]interface{} {
	result := make(map[string]interface{})

	lines := strings.Split(output, "\n")
	var currentSection string
	var currentData map[string]interface{}

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// Check if this is a section header
		if !strings.HasPrefix(line, "\t") && strings.Contains(line, "Information") {
			if currentSection != "" && currentData != nil {
				result[currentSection] = currentData
			}
			currentSection = line
			currentData = make(map[string]interface{})
			continue
		}

		// Parse key-value pairs
		if strings.Contains(line, ":") && currentData != nil {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				currentData[key] = value
			}
		}
	}

	// Add the last section
	if currentSection != "" && currentData != nil {
		result[currentSection] = currentData
	}

	return result
}

// Network interface management functions
func getCurrentNetworkInterfaces() ([]NetworkInterface, error) {
	var interfaces []NetworkInterface

	// Get network interfaces using 'ip' command
	cmd := exec.Command("ip", "addr", "show")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	var currentInterface *NetworkInterface

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Parse interface name and state
		if strings.Contains(line, ": ") && !strings.HasPrefix(line, " ") {
			if currentInterface != nil {
				interfaces = append(interfaces, *currentInterface)
			}

			// Extract interface name
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				name := strings.TrimSpace(parts[1])
				currentInterface = &NetworkInterface{Name: name}

				// Extract state
				if strings.Contains(line, "state UP") {
					currentInterface.State = "UP"
				} else if strings.Contains(line, "state DOWN") {
					currentInterface.State = "DOWN"
				}
			}
		}

		// Parse MAC address
		if currentInterface != nil && strings.Contains(line, "link/ether") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				currentInterface.MAC = strings.ToUpper(parts[1])
			}
		}

		// Parse IP address
		if currentInterface != nil && strings.Contains(line, "inet ") && !strings.Contains(line, "127.0.0.1") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ip := strings.Split(parts[1], "/")[0]
				currentInterface.IP = ip
			}
		}
	}

	// Add the last interface
	if currentInterface != nil {
		interfaces = append(interfaces, *currentInterface)
	}

	// Get driver information for each interface
	for i := range interfaces {
		if driver, err := getInterfaceDriver(interfaces[i].Name); err == nil {
			interfaces[i].Driver = driver
		}
	}

	return interfaces, nil
}

func getInterfaceDriver(interfaceName string) (string, error) {
	// Try ethtool first
	cmd := exec.Command("ethtool", "-i", interfaceName)
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "driver:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1]), nil
				}
			}
		}
	}

	// Fallback: check /sys/class/net
	driverPath := fmt.Sprintf("/sys/class/net/%s/device/driver", interfaceName)
	if link, err := os.Readlink(driverPath); err == nil {
		return filepath.Base(link), nil
	}

	return "", fmt.Errorf("driver not found for interface %s", interfaceName)
}

func getIntelNetworkDrivers() ([]string, error) {
	printInfo("Detecting Intel network drivers...")

	// Получаем список всех Intel сетевых карт через lspci
	cmd := exec.Command("lspci", "-nn", "-d", "8086:")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run lspci: %v", err)
	}

	var drivers []string
	driverSet := make(map[string]bool) // Для удаления дубликатов

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Ищем сетевые контроллеры (Ethernet controller, Network controller)
		if strings.Contains(strings.ToLower(line), "ethernet") ||
			strings.Contains(strings.ToLower(line), "network") {

			// Извлекаем PCI адрес (первая часть строки до пробела)
			parts := strings.Fields(line)
			if len(parts) == 0 {
				continue
			}
			pciAddr := parts[0]

			// Получаем драйвер для этого устройства
			driverPath := fmt.Sprintf("/sys/bus/pci/devices/0000:%s/driver", pciAddr)
			if link, err := os.Readlink(driverPath); err == nil {
				driverName := filepath.Base(link)
				if !driverSet[driverName] {
					drivers = append(drivers, driverName)
					driverSet[driverName] = true
					printInfo(fmt.Sprintf("Found Intel driver: %s (PCI: %s)", driverName, pciAddr))
				}
			}
		}
	}

	if len(drivers) == 0 {
		printWarning("No Intel network drivers found, trying common drivers...")
		// Fallback к общим Intel драйверам
		commonDrivers := []string{"igb", "e1000e", "ixgbe", "i40e", "ice"}
		for _, driver := range commonDrivers {
			// Проверяем, загружен ли драйвер
			cmd := exec.Command("lsmod")
			output, err := cmd.Output()
			if err == nil && strings.Contains(string(output), driver) {
				drivers = append(drivers, driver)
				printInfo(fmt.Sprintf("Found loaded Intel driver: %s", driver))
			}
		}
	}

	printSuccess(fmt.Sprintf("Detected %d Intel network driver(s)", len(drivers)))
	return drivers, nil
}

func normalizeMAC(mac string) string {
	// Remove any separators and convert to uppercase
	mac = strings.ReplaceAll(mac, ":", "")
	mac = strings.ReplaceAll(mac, "-", "")
	mac = strings.ToUpper(mac)

	// Add colons in standard format
	if len(mac) == 12 {
		return fmt.Sprintf("%s:%s:%s:%s:%s:%s",
			mac[0:2], mac[2:4], mac[4:6], mac[6:8], mac[8:10], mac[10:12])
	}

	return mac
}

func isTargetMACPresent(targetMAC string, interfaces []NetworkInterface) (bool, string) {
	normalizedTarget := normalizeMAC(targetMAC)

	for _, iface := range interfaces {
		if normalizeMAC(iface.MAC) == normalizedTarget {
			return true, iface.Name
		}
	}

	return false, ""
}

func askFlashRetryAction(message string) string {
	fmt.Printf("\n%s=== MAC FLASHING ERROR ===%s\n", ColorRed, ColorReset)
	fmt.Printf("%s\n", message)
	fmt.Println("Choose action:")
	fmt.Printf("  %s[Y]%s Yes - Retry flashing (default)\n", ColorGreen, ColorReset)
	fmt.Printf("  %s[A]%s Abort - Stop flashing and continue program\n", ColorYellow, ColorReset)
	fmt.Printf("  %s[S]%s Skip - Skip MAC flashing by operator decision\n", ColorBlue, ColorReset)
	fmt.Printf("Choice [Y/a/s]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "RETRY" // default on error
	}

	choice := strings.ToUpper(strings.TrimSpace(input))
	if choice == "" {
		choice = "Y" // default
	}

	switch choice {
	case "Y", "YES":
		return "RETRY"
	case "A", "ABORT":
		return "ABORT"
	case "S", "SKIP":
		return "SKIP"
	default:
		fmt.Printf("Invalid choice '%s', defaulting to retry.\n", choice)
		return "RETRY"
	}
}

func flashMAC(flashConfig FlashConfig, systemConfig SystemConfig, mac string) error {
	method := flashConfig.Method
	if method == "" {
		method = "eeupdate" // default
	}

	printSubHeader("MAC ADDRESS FLASHING", fmt.Sprintf("Method: %s | Target MAC: %s", method, mac))

	// Step 1: Get current network interfaces and save original MACs
	interfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		return fmt.Errorf("failed to get network interfaces: %v", err)
	}

	// Log original MAC addresses before flashing
	printInfo("Original MAC addresses before flashing:")
	for _, iface := range interfaces {
		if iface.MAC != "" && iface.Name != "lo" {
			printInfo(fmt.Sprintf("  %s: %s [%s]", iface.Name, iface.MAC, iface.Driver))
		}
	}

	// Step 2: Check if target MAC already exists
	exists, interfaceName := isTargetMACPresent(mac, interfaces)
	if exists {
		printSuccess(fmt.Sprintf("Target MAC %s already present on interface %s - skipping flash", mac, interfaceName))
		return nil
	}

	// Step 3: Show current network state
	fmt.Printf("\nCurrent network interfaces:\n")
	for _, iface := range interfaces {
		status := "DOWN"
		if iface.State == "UP" {
			status = fmt.Sprintf("UP (IP: %s)", iface.IP)
		}
		fmt.Printf("  %s: %s [%s] - %s\n", iface.Name, iface.MAC, iface.Driver, status)
	}

	// Step 4: Execute flashing based on method
	var summary FlashMACSummary
	summary.Method = method
	summary.TargetMAC = mac

	switch method {
	case "rtnicpg":
		err = flashMACWithRtnicpg(mac, interfaces, systemConfig, &summary)
	case "eeupdate":
		err = flashMACWithEeupdate(mac, interfaces, flashConfig, &summary)
	default:
		return fmt.Errorf("unknown flash method: %s", method)
	}

	if err != nil {
		return fmt.Errorf("MAC flashing failed: %v", err)
	}

	if summary.Success {
		printSuccess(fmt.Sprintf("MAC address flashed successfully using %s method", method))
	}

	return nil
}

func discoverIntelNICs(venDeviceFilter []string) ([]IntelNIC, error) {
	printInfo("Discovering Intel network cards...")

	cmd := exec.Command("eeupdate64e", "/MAC_DUMP_ALL")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// Check if command failed completely (exit codes other than 2 are critical)
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ExitCode()
			if exitCode == 2 {
				// Exit code 2 usually means no driver found, but utility can still work
				printInfo("eeupdate64e reports no driver (exit code 2), but continuing...")
			} else {
				// Other exit codes are more serious errors
				return nil, fmt.Errorf("eeupdate64e discovery failed with exit code %d: %v\nOutput: %s", exitCode, err, outputStr)
			}
		} else {
			// Non-ExitError (like command not found)
			return nil, fmt.Errorf("eeupdate64e discovery failed: %v\nOutput: %s", err, outputStr)
		}
	}

	// Parse output to find NIC indices regardless of exit code
	var allNICs []IntelNIC
	lines := strings.Split(outputStr, "\n")

	for _, line := range lines {
		// Parse lines with device IDs (8086-XXXX format indicates Intel)
		if strings.Contains(line, "8086-") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				// First field should be NIC index
				nicIndex, err := strconv.Atoi(fields[0])
				if err != nil {
					continue
				}

				// Extract vendor-device ID (format: 8086-1521)
				venDevice := fields[4]
				description := strings.Join(fields[5:], " ")

				nic := IntelNIC{
					Index:        nicIndex,
					VendorDevice: venDevice,
					Description:  description,
				}

				allNICs = append(allNICs, nic)
				printInfo(fmt.Sprintf("Found Intel NIC %d: %s (%s)", nicIndex, venDevice, description))
			}
		}
	}

	if len(allNICs) == 0 {
		// If no NICs found in parsing, but we got output, try common indices
		if len(outputStr) > 100 { // Substantial output suggests NICs might be there
			printInfo("No NICs found in parsing, but substantial output detected. Trying common indices...")
			for i := 1; i <= 6; i++ {
				allNICs = append(allNICs, IntelNIC{Index: i, VendorDevice: "unknown", Description: "Unknown Intel NIC"})
			}
		} else {
			return nil, fmt.Errorf("no Intel network cards found in output")
		}
	}

	// Apply vendor-device filter if specified
	var filteredNICs []IntelNIC
	if len(venDeviceFilter) > 0 {
		printInfo(fmt.Sprintf("Applying vendor-device filter: %s", strings.Join(venDeviceFilter, ", ")))
		for _, nic := range allNICs {
			for _, filter := range venDeviceFilter {
				if nic.VendorDevice == filter {
					filteredNICs = append(filteredNICs, nic)
					printInfo(fmt.Sprintf("NIC %d matches filter %s", nic.Index, filter))
					break
				}
			}
		}
		if len(filteredNICs) == 0 {
			return nil, fmt.Errorf("no NICs match the specified vendor-device filter: %s", strings.Join(venDeviceFilter, ", "))
		}
	} else {
		filteredNICs = allNICs
	}

	printSuccess(fmt.Sprintf("Discovery completed: found %d Intel NIC(s) (after filtering)", len(filteredNICs)))
	return filteredNICs, nil
}

// incrementMAC increases MAC address by 1 (handles hexadecimal arithmetic)
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

func executeEeupdateFlashing(nicIndex int, targetMAC string) error {

	cleanMac := strings.ReplaceAll(targetMAC, ":", "")

	printInfo(fmt.Sprintf("Executing eeupdate flashing for NIC %d, MAC: %s", nicIndex, targetMAC))

	// Execute eeupdate64e with NIC and MAC parameters
	cmd := exec.Command("eeupdate64e",
		fmt.Sprintf("/NIC=%d", nicIndex),
		fmt.Sprintf("/MAC=%s", cleanMac))

	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// Get exit code for detailed error reporting
	var exitCode int = 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		}
	}

	// Handle exit codes specifically
	if err != nil {
		if exitCode == 2 {
			// Exit code 2 usually means no driver, but flashing might still work
			printInfo(fmt.Sprintf("eeupdate64e reports no driver (exit code 2) for NIC %d, checking output for success...", nicIndex))
		} else {
			// Other exit codes might be more serious
			printError(fmt.Sprintf("eeupdate64e failed with exit code %d for NIC %d", exitCode, nicIndex))
			printError(fmt.Sprintf("Output: %s", outputStr))
			return fmt.Errorf("eeupdate64e command failed with exit code %d: %v", exitCode, err)
		}
	}

	// Check output for success/failure indicators regardless of exit code
	outputLower := strings.ToLower(outputStr)

	// Look for specific success patterns from eeupdate
	if strings.Contains(outputStr, "Updating Mac Address") && strings.Contains(outputStr, "Done") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	if strings.Contains(outputStr, "Updating Checksum and CRCs") && strings.Contains(outputStr, "Done") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	// Other positive indicators
	if strings.Contains(outputLower, "success") ||
		strings.Contains(outputLower, "complete") ||
		strings.Contains(outputLower, "updated") ||
		strings.Contains(outputLower, "written") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	// Negative indicators (but exclude our own error headers)
	if (strings.Contains(outputLower, "error") && !strings.Contains(outputLower, "mac flashing error")) ||
		strings.Contains(outputLower, "fail") ||
		strings.Contains(outputLower, "invalid") {
		return fmt.Errorf("eeupdate reported error for NIC %d (exit code %d): %s", nicIndex, exitCode, outputStr)
	}

	// If no clear indicators but we got substantial output, assume it worked
	if len(outputStr) > 50 && err == nil {
		printSuccess(fmt.Sprintf("eeupdate command completed for NIC %d", nicIndex))
		return nil
	}

	// If exit code 2 but minimal output, still try to continue
	if err != nil && exitCode == 2 {
		printInfo(fmt.Sprintf("eeupdate completed for NIC %d with driver warning (exit code 2)", nicIndex))
		return nil
	}

	// Default case - if we get here, status is unclear
	printInfo(fmt.Sprintf("eeupdate command status unclear for NIC %d (exit code %d), assuming success", nicIndex, exitCode))
	return nil
}

func flashMACWithEeupdate(targetMAC string, interfaces []NetworkInterface, flashConfig FlashConfig, summary *FlashMACSummary) error {
	printInfo("Starting eeupdate MAC flashing process...")

	// Step 1: Save current IP
	var originalIP string
	for _, iface := range interfaces {
		if iface.IP != "" && iface.State == "UP" {
			originalIP = iface.IP
			break
		}
	}
	summary.OriginalIP = originalIP

	if originalIP != "" {
		printInfo(fmt.Sprintf("Current IP address saved: %s", originalIP))
	}

	// Step 2: Get Intel network drivers before discovery
	intelDrivers, err := getIntelNetworkDrivers()
	if err != nil {
		printWarning(fmt.Sprintf("Failed to detect Intel drivers: %v", err))
		intelDrivers = []string{"igb"} // Fallback к наиболее распространенному
	}

	// Step 3: Discover Intel NICs with optional filtering
	printInfo("Scanning for Intel network cards...")
	intelNICs, err := discoverIntelNICs(flashConfig.VenDevice)
	if err != nil {
		return fmt.Errorf("failed to discover Intel NICs: %v", err)
	}

	if len(intelNICs) == 0 {
		return fmt.Errorf("no Intel network cards found")
	}

	// Extract indices for summary
	var nicIndices []int
	for _, nic := range intelNICs {
		nicIndices = append(nicIndices, nic.Index)
	}
	summary.NICIndices = nicIndices

	printSuccess(fmt.Sprintf("Found %d Intel NIC(s) for flashing:", len(intelNICs)))
	for i, nic := range intelNICs {
		// Calculate MAC for this NIC (first gets original, others get incremented)
		currentMAC := targetMAC
		if i > 0 {
			for j := 0; j < i; j++ {
				currentMAC, err = incrementMAC(currentMAC)
				if err != nil {
					return fmt.Errorf("failed to increment MAC address for NIC %d: %v", nic.Index, err)
				}
			}
		}
		fmt.Printf("  NIC %d: %s (%s) -> MAC: %s\n", nic.Index, nic.VendorDevice, nic.Description, currentMAC)
	}

	// Step 4: Unload Intel drivers before flashing
	printInfo("Unloading Intel network drivers for flashing...")
	for _, driver := range intelDrivers {
		if err := unloadNetworkDriver(driver); err != nil {
			printWarning(fmt.Sprintf("Failed to unload driver %s: %v", driver, err))
		} else {
			printSuccess(fmt.Sprintf("Driver %s unloaded successfully", driver))
		}
	}

	// Wait for drivers to fully unload
	time.Sleep(2 * time.Second)

	// Step 5: Flash each NIC with incremented MAC addresses
	attempts := 0
	maxAttempts := 3
	var lastError error

	for attempts < maxAttempts {
		attempts++
		printInfo(fmt.Sprintf("Flashing attempt %d/%d...", attempts, maxAttempts))

		success := true
		flashedNICs := 0

		for i, nic := range intelNICs {
			// Calculate MAC for this NIC
			currentMAC := targetMAC
			if i > 0 {
				for j := 0; j < i; j++ {
					currentMAC, err = incrementMAC(currentMAC)
					if err != nil {
						lastError = fmt.Errorf("failed to increment MAC address for NIC %d: %v", nic.Index, err)
						success = false
						break
					}
				}
			}

			if !success {
				break
			}

			printInfo(fmt.Sprintf("Flashing NIC %d (%s) with MAC %s...", nic.Index, nic.VendorDevice, currentMAC))
			if err := executeEeupdateFlashing(nic.Index, currentMAC); err != nil {
				printError(fmt.Sprintf("Failed to flash NIC %d: %v", nic.Index, err))
				lastError = fmt.Errorf("failed to flash NIC %d: %v", nic.Index, err)
				success = false
				break
			} else {
				flashedNICs++
				printSuccess(fmt.Sprintf("NIC %d flashing completed with MAC %s", nic.Index, currentMAC))
			}
		}

		if success {
			printSuccess(fmt.Sprintf("All %d NICs flashed successfully with incremented MAC addresses", flashedNICs))
			lastError = nil
			break
		}

		if attempts < maxAttempts {
			action := askFlashRetryAction(fmt.Sprintf("eeupdate flashing failed (attempt %d/%d): %v", attempts, maxAttempts, lastError))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "Skipped by operator"
				// Reload drivers before exiting
				reloadIntelDrivers(intelDrivers)
				return nil
			}
			if action == "ABORT" {
				summary.Success = false
				summary.Error = fmt.Sprintf("Aborted by operator after %d attempts", attempts)
				// Reload drivers before exiting
				reloadIntelDrivers(intelDrivers)
				return fmt.Errorf("flashing aborted by operator")
			}
			// Continue to retry if action == "RETRY"
		}
	}

	if lastError != nil && attempts >= maxAttempts {
		summary.Success = false
		summary.Error = fmt.Sprintf("Max attempts reached: %v", lastError)
		// Reload drivers before exiting
		reloadIntelDrivers(intelDrivers)
		return lastError
	}

	// Step 6: Reload Intel drivers after flashing
	printInfo("Reloading Intel network drivers...")
	reloadIntelDrivers(intelDrivers)

	// Wait for drivers to fully load and interfaces to come up
	time.Sleep(5 * time.Second)

	// Step 7: Verify that at least the first MAC address is present
	printInfo("Verifying MAC address presence...")
	newInterfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		printError(fmt.Sprintf("Warning: failed to verify MAC flashing: %v", err))
	} else {
		// Check for the primary MAC address (first one)
		exists, interfaceName := isTargetMACPresent(targetMAC, newInterfaces)
		if exists {
			summary.Success = true
			summary.InterfaceName = interfaceName
			printSuccess(fmt.Sprintf("SUCCESS: Primary MAC %s found on interface %s", targetMAC, interfaceName))

			// Also check for incremented MAC addresses and report them
			currentMAC := targetMAC
			for i := 1; i < len(intelNICs); i++ {
				currentMAC, err = incrementMAC(currentMAC)
				if err != nil {
					printError(fmt.Sprintf("Warning: failed to increment MAC for verification: %v", err))
					break
				}

				exists, ifaceName := isTargetMACPresent(currentMAC, newInterfaces)
				if exists {
					printSuccess(fmt.Sprintf("Additional MAC %s found on interface %s", currentMAC, ifaceName))
				} else {
					printError(fmt.Sprintf("Warning: Expected MAC %s not found on any interface", currentMAC))
				}
			}

			// Try to restore IP address to the primary interface
			if originalIP != "" {
				printInfo(fmt.Sprintf("Restoring original IP address: %s", originalIP))
				if err := restoreIPAddress(interfaceName, originalIP); err != nil {
					printError(fmt.Sprintf("Warning: failed to restore IP %s: %v", originalIP, err))
				} else {
					printSuccess(fmt.Sprintf("IP address %s restored successfully", originalIP))
				}
			}
		} else {
			printError("Primary MAC not found on any interface after flashing")
			action := askFlashRetryAction(fmt.Sprintf("Flashing completed but target MAC %s not found on any interface", targetMAC))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "MAC not found after flashing - skipped by operator"
				return nil
			}
			if action == "ABORT" {
				summary.Success = false
				summary.Error = "MAC not found after flashing - aborted by operator"
				return fmt.Errorf("MAC not found after flashing - aborted by operator")
			}
			summary.Success = false
			summary.Error = "MAC not found after flashing"
			return fmt.Errorf("target MAC not found after flashing")
		}
	}

	return nil
}

// Функция для проверки загрузки pgdrv модуля с таймаутом
func verifyPgdrvLoaded() error {
	cmd := exec.Command("lsmod")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run lsmod: %v", err)
	}

	if strings.Contains(string(output), "pgdrv") {
		return nil
	}

	return fmt.Errorf("pgdrv module not found in lsmod output")
}

// Функция ожидания загрузки pgdrv с циклом проверки
func waitForPgdrvLoad(timeoutSeconds int) error {
	for i := 0; i < timeoutSeconds*10; i++ { // Проверяем каждые 100мс
		if err := verifyPgdrvLoaded(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond) // Задержка цикла проверки
	}
	return fmt.Errorf("timeout waiting for pgdrv module to load")
}

// Функция ожидания выгрузки pgdrv с циклом проверки
func waitForPgdrvUnload(timeoutSeconds int) error {
	for i := 0; i < timeoutSeconds*10; i++ { // Проверяем каждые 100мс
		if err := verifyPgdrvLoaded(); err != nil {
			return nil // Модуль не найден = выгружен
		}
		time.Sleep(100 * time.Millisecond) // Задержка цикла проверки
	}
	return fmt.Errorf("timeout waiting for pgdrv module to unload")
}

// Функция для загрузки rtnicpg драйвера из файла
func loadRtnicpgDriverFromPath(driverPath string) error {
	printInfo(fmt.Sprintf("Loading rtnicpg driver from: %s", driverPath))

	// Проверяем существование файла
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return fmt.Errorf("driver file not found: %s", driverPath)
	}

	// Загружаем драйвер
	cmd := exec.Command("insmod", driverPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("insmod failed: %v\nOutput: %s", err, string(output))
	}

	// Ждем загрузки pgdrv модуля с таймаутом
	if err := waitForPgdrvLoad(5); err != nil {
		return fmt.Errorf("pgdrv driver verification failed: %v", err)
	}

	printSuccess("pgdrv driver loaded and verified successfully")
	return nil
}

// Функция для выгрузки pgdrv модуля
func unloadPgdrvDriver() error {
	printInfo("Unloading pgdrv module")

	// Проверяем, загружен ли pgdrv
	if err := verifyPgdrvLoaded(); err != nil {
		printInfo("pgdrv module not loaded, nothing to unload")
		return nil
	}

	// Выгружаем модуль pgdrv
	cmd := exec.Command("rmmod", "pgdrv")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Если не получилось, попробуем форсированно
		printWarning(fmt.Sprintf("Normal rmmod failed, trying force: %v", err))
		cmd = exec.Command("rmmod", "-f", "pgdrv")
		output, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("rmmod pgdrv failed: %v\nOutput: %s", err, string(output))
		}
	}

	// Ждем выгрузки модуля с таймаутом
	if err := waitForPgdrvUnload(3); err != nil {
		printWarning("pgdrv module still appears loaded after rmmod")
	} else {
		printSuccess("pgdrv module unloaded successfully")
	}

	return nil
}

// Функция ожидания загрузки сетевого драйвера
func waitForDriverLoad(driverName string, timeoutSeconds int) error {
	for i := 0; i < timeoutSeconds*10; i++ { // Проверяем каждые 100мс
		cmd := exec.Command("lsmod")
		output, err := cmd.Output()
		if err == nil && strings.Contains(string(output), driverName) {
			return nil
		}
		time.Sleep(100 * time.Millisecond) // Задержка цикла проверки
	}
	return fmt.Errorf("timeout waiting for driver %s to load", driverName)
}

// Функция для проверки первоначального состояния драйверов
func checkInitialDriverState(primaryInterface *NetworkInterface) (pgdrvLoaded bool, realtekActive bool) {
	// Проверяем загружен ли pgdrv
	pgdrvLoaded = (verifyPgdrvLoaded() == nil)

	// Проверяем активен ли Realtek драйвер
	realtekActive = false
	if primaryInterface != nil && primaryInterface.Driver != "" && isRealtekDriver(primaryInterface.Driver) {
		cmd := exec.Command("lsmod")
		if output, err := cmd.Output(); err == nil {
			realtekActive = strings.Contains(string(output), primaryInterface.Driver)
		}
	}

	return pgdrvLoaded, realtekActive
}

// Заменяем функцию loadFlashingDriver на версию без хардкодных sleep'ов
func loadFlashingDriver(driverDir, originalDriver string) (string, error) {
	printInfo(fmt.Sprintf("Loading flashing driver for: %s", originalDriver))

	// Получаем версию ядра
	kernelVersion, err := getKernelVersion()
	if err != nil {
		return "", fmt.Errorf("failed to get kernel version: %v", err)
	}

	// Step 1: Проверяем наличие готового скомпилированного драйвера
	compiledDriverPath, found := checkCompiledDriver(driverDir, originalDriver, kernelVersion)
	if found {
		printInfo("Attempting to use pre-compiled rtnicpg driver...")
		if err := loadRtnicpgDriverFromPath(compiledDriverPath); err == nil {
			printSuccess("Pre-compiled pgdrv driver loaded successfully")
			return compiledDriverPath, nil
		} else {
			printWarning(fmt.Sprintf("Pre-compiled driver failed to load: %v", err))
			printInfo("Will attempt to recompile driver...")

			// Убираем возможно частично загруженный модуль
			unloadPgdrvDriver()
		}
	}

	// Step 2: Компилируем новый драйвер
	printInfo("Compiling new rtnicpg driver...")
	compiledPath, err := compileFlashingDriver(driverDir, originalDriver)
	if err != nil {
		return "", fmt.Errorf("failed to compile driver: %v", err)
	}

	// Step 3: Загружаем новый драйвер
	if err := loadRtnicpgDriverFromPath(compiledPath); err != nil {
		return "", fmt.Errorf("failed to load compiled pgdrv driver: %v", err)
	}

	printSuccess("rtnicpg driver compiled and pgdrv module loaded successfully")
	return compiledPath, nil
}

// Модифицированная функция flashMACWithRtnicpg для работы с Realtek драйверами
func flashMACWithRtnicpg(targetMAC string, interfaces []NetworkInterface, systemConfig SystemConfig, summary *FlashMACSummary) error {
	printInfo("Starting rtnicpg MAC flashing process with Realtek driver detection...")

	// Диагностика интерфейсов для отладки
	debugNetworkInterfaces(interfaces)
	debugLoadedModules()

	// Step 1: Сначала попытаемся найти Realtek интерфейс
	primaryInterface := findRealtekInterface(interfaces)

	// Step 1.1: Если Realtek не найден, используем fallback на старую логику
	if primaryInterface == nil {
		printWarning("No Realtek network interface found, using fallback to any active interface...")
		printInfo("Available interfaces:")
		for _, iface := range interfaces {
			if iface.Name != "lo" {
				driverType := "UNKNOWN"
				if iface.Driver != "" {
					if isRealtekDriver(iface.Driver) {
						driverType = "REALTEK"
					} else if strings.Contains(strings.ToLower(iface.Driver), "intel") ||
						iface.Driver == "igb" || iface.Driver == "e1000e" ||
						iface.Driver == "ixgbe" || iface.Driver == "i40e" || iface.Driver == "ice" {
						driverType = "INTEL"
					} else {
						driverType = "OTHER"
					}
				}
				printInfo(fmt.Sprintf("  [%s] %s: MAC=%s Driver=%s State=%s IP=%s",
					driverType, iface.Name, iface.MAC, iface.Driver, iface.State, iface.IP))
			}
		}

		// Fallback: ищем любой активный интерфейс с IP (как в оригинальном коде)
		for i := range interfaces {
			if interfaces[i].IP != "" && interfaces[i].State == "UP" {
				primaryInterface = &interfaces[i]
				printWarning(fmt.Sprintf("Using fallback interface %s (Driver: %s) - rtnicpg may work with non-Realtek drivers",
					interfaces[i].Name, interfaces[i].Driver))
				break
			}
		}

		if primaryInterface == nil {
			return fmt.Errorf("no active network interface with IP found")
		}
	}

	summary.OriginalIP = primaryInterface.IP
	summary.OriginalDriver = primaryInterface.Driver

	printInfo(fmt.Sprintf("Using interface %s (IP: %s, Driver: %s, State: %s)",
		primaryInterface.Name, primaryInterface.IP, primaryInterface.Driver, primaryInterface.State))

	// Step 2: Если интерфейс неактивен, попытаемся его поднять (но не будем ждать)
	if primaryInterface.State != "UP" {
		printInfo(fmt.Sprintf("Interface %s is DOWN, attempting to bring it UP...", primaryInterface.Name))
		cmd := exec.Command("ip", "link", "set", primaryInterface.Name, "up")
		if err := cmd.Run(); err != nil {
			printWarning(fmt.Sprintf("Failed to bring interface UP: %v", err))
		} else {
			printInfo(fmt.Sprintf("Interface %s UP command sent (not waiting for activation)", primaryInterface.Name))
		}
	}

	// Step 3: Подготовка pgdrv драйвера с проверкой начального состояния
	driverPath, err := preparePgdrvDriver(systemConfig.DriverDir, primaryInterface.Driver, primaryInterface)
	if err != nil {
		// Try to restore original driver if preparation failed
		printWarning("Failed to prepare pgdrv driver, attempting to restore original...")
		if restoreErr := loadNetworkDriver(primaryInterface.Driver); restoreErr != nil {
			printError(fmt.Sprintf("Failed to restore original driver: %v", restoreErr))
		}
		return fmt.Errorf("failed to prepare pgdrv driver: %v", err)
	}

	// Step 3.1: Verify pgdrv is loaded
	if err := verifyPgdrvLoaded(); err != nil {
		// Try to restore original driver
		printError("pgdrv module not found after preparation, restoring original driver...")
		loadNetworkDriver(primaryInterface.Driver)
		return fmt.Errorf("pgdrv module verification failed: %v", err)
	}
	printSuccess("pgdrv module confirmed loaded and ready for flashing")

	// Step 4: Flash MAC using rtnic
	attempts := 0
	maxAttempts := 3
	var flashErr error

	for attempts < maxAttempts {
		attempts++
		printInfo(fmt.Sprintf("Flashing MAC attempt %d/%d using rtnic (pgdrv loaded)...", attempts, maxAttempts))

		flashErr = executeRtnicFlashing(targetMAC)
		if flashErr == nil {
			printSuccess(fmt.Sprintf("rtnic flashing completed successfully on attempt %d", attempts))
			break
		}

		printError(fmt.Sprintf("rtnic flashing failed on attempt %d: %v", attempts, flashErr))

		if attempts < maxAttempts {
			action := askFlashRetryAction(fmt.Sprintf("rtnic flashing failed (attempt %d): %v", attempts, flashErr))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "Skipped by operator"
				break
			}
			if action == "ABORT" {
				summary.Success = false
				summary.Error = "Aborted by operator"
				flashErr = fmt.Errorf("flashing aborted by operator")
				break
			}
			if action != "RETRY" {
				break
			}
		}
	}

	// Step 5: Cleanup - unload pgdrv module and restore original driver
	printInfo("Cleaning up: unloading pgdrv and restoring original driver...")

	// Выгружаем pgdrv модуль (если он не был предзагружен)
	if driverPath != "pgdrv_already_loaded" {
		if err := unloadPgdrvDriver(); err != nil {
			printError(fmt.Sprintf("Warning: failed to unload pgdrv module: %v", err))
		}

		// Восстанавливаем оригинальный драйвер
		if err := loadNetworkDriver(primaryInterface.Driver); err != nil {
			printError(fmt.Sprintf("Warning: failed to restore original driver %s: %v", primaryInterface.Driver, err))
		} else {
			printSuccess(fmt.Sprintf("Original driver %s restored successfully", primaryInterface.Driver))
		}
	} else {
		printInfo("pgdrv was pre-loaded, leaving it active (not restoring original driver)")
	}

	// Step 5.1: Verify cleanup state
	debugLoadedModules()

	// Проверяем результат флэширования
	if flashErr != nil && attempts >= maxAttempts {
		summary.Success = false
		summary.Error = fmt.Sprintf("Max attempts reached: %v", flashErr)
		return flashErr
	}

	if summary.Error != "" {
		return fmt.Errorf(summary.Error)
	}

	// Step 6: Verify MAC was flashed
	printInfo("Verifying MAC address after flashing...")

	newInterfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		printError(fmt.Sprintf("Warning: failed to verify MAC flashing: %v", err))
		summary.Success = false
		summary.Error = "Failed to verify flashing result"
		return fmt.Errorf("failed to verify MAC flashing: %v", err)
	}

	// Проверяем наличие целевого MAC адреса
	exists, interfaceName := isTargetMACPresent(targetMAC, newInterfaces)
	if exists {
		summary.Success = true
		summary.InterfaceName = interfaceName
		printSuccess(fmt.Sprintf("SUCCESS: MAC %s found on interface %s", targetMAC, interfaceName))

		// Попытаемся восстановить IP адрес, если он был
		if summary.OriginalIP != "" {
			printInfo(fmt.Sprintf("Attempting to restore original IP address: %s", summary.OriginalIP))
			if err := restoreIPAddress(interfaceName, summary.OriginalIP); err != nil {
				printWarning(fmt.Sprintf("Failed to restore IP %s: %v", summary.OriginalIP, err))
			} else {
				printSuccess(fmt.Sprintf("IP address %s restored successfully", summary.OriginalIP))
			}
		}

		// Проверяем, что интерфейс активен
		for _, iface := range newInterfaces {
			if iface.Name == interfaceName {
				if iface.State != "UP" {
					printInfo(fmt.Sprintf("Bringing interface %s UP...", interfaceName))
					cmd := exec.Command("ip", "link", "set", interfaceName, "up")
					cmd.Run()
				}
				break
			}
		}
	} else {
		printError(fmt.Sprintf("FAILURE: Target MAC %s not found on any interface after flashing", targetMAC))

		// Показываем текущие MAC адреса для отладки
		printInfo("Current MAC addresses after flashing:")
		for _, iface := range newInterfaces {
			if iface.MAC != "" && iface.Name != "lo" {
				driverType := "OTHER"
				if isRealtekDriver(iface.Driver) {
					driverType = "REALTEK"
				}
				printInfo(fmt.Sprintf("  [%s] %s: %s", driverType, iface.Name, iface.MAC))
			}
		}

		action := askFlashRetryAction(fmt.Sprintf("Flashing completed but target MAC %s not found on any interface", targetMAC))
		if action == "SKIP" {
			summary.Success = false
			summary.Error = "MAC not found after flashing - skipped by operator"
			return nil
		}
		if action == "ABORT" {
			summary.Success = false
			summary.Error = "MAC not found after flashing - aborted by operator"
			return fmt.Errorf("MAC not found after flashing - aborted by operator")
		}
		summary.Success = false
		summary.Error = "MAC not found after flashing"
		return fmt.Errorf("target MAC not found after flashing")
	}

	return nil
}

// Диагностическая функция для отладки модулей
func debugLoadedModules() {
	printInfo("=== Loaded Network Modules Debug ===")

	cmd := exec.Command("lsmod")
	output, err := cmd.Output()
	if err != nil {
		printError(fmt.Sprintf("Failed to run lsmod: %v", err))
		return
	}

	lines := strings.Split(string(output), "\n")
	printInfo("Network-related modules:")

	pgdrvFound := false
	for _, line := range lines[1:] { // Skip header
		if strings.Contains(line, "r8") ||
			strings.Contains(line, "rtl") ||
			strings.Contains(line, "8139") ||
			strings.Contains(line, "igb") ||
			strings.Contains(line, "e1000") ||
			strings.Contains(line, "ixgbe") ||
			strings.Contains(line, "i40e") ||
			strings.Contains(line, "ice") ||
			strings.Contains(line, "pgdrv") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				status := ""
				if parts[0] == "pgdrv" {
					status = " ← RTNICPG FLASHING DRIVER"
					pgdrvFound = true
				}
				printInfo(fmt.Sprintf("  %s (used by %s, refs: %s)%s", parts[0], parts[2], parts[1], status))
			}
		}
	}

	if pgdrvFound {
		printSuccess("pgdrv module is currently loaded")
	} else {
		printInfo("pgdrv module is not loaded")
	}

	printInfo("=== End Module Debug ===")
}

// Функция для генерации имени файла драйвера
func getDriverFileName(driverName, kernelVersion string) string {
	return fmt.Sprintf("%s_%s.ko", driverName, kernelVersion)
}

// Функция для проверки существования скомпилированного драйвера
func checkCompiledDriver(driverDir, driverName, kernelVersion string) (string, bool) {
	driverFileName := getDriverFileName(driverName, kernelVersion)
	driverPath := filepath.Join(driverDir, driverFileName)

	if _, err := os.Stat(driverPath); err == nil {
		printInfo(fmt.Sprintf("Found compiled driver: %s", driverPath))
		return driverPath, true
	}

	return "", false
}

// Функция для проверки исходников драйвера rtnicpg
func checkRtnicpgSources(driverDir string) (string, bool) {
	rtnicpgDir := filepath.Join(driverDir, "rtnicpg")
	makefilePath := filepath.Join(rtnicpgDir, "Makefile")

	// Проверяем существование папки rtnicpg
	if _, err := os.Stat(rtnicpgDir); os.IsNotExist(err) {
		return "", false
	}

	// Проверяем существование Makefile
	if _, err := os.Stat(makefilePath); os.IsNotExist(err) {
		return "", false
	}

	printInfo(fmt.Sprintf("Found rtnicpg sources: %s", rtnicpgDir))
	return rtnicpgDir, true
}

// Функция для проверки требований к сборке
func checkBuildRequirements() error {
	printInfo("Checking build requirements...")

	// Проверяем наличие make
	if _, err := exec.LookPath("make"); err != nil {
		return fmt.Errorf("make not found - install build-essential package")
	}

	// Проверяем наличие компилятора
	if _, err := exec.LookPath("gcc"); err != nil {
		return fmt.Errorf("gcc not found - install build-essential package")
	}

	// Проверяем наличие заголовков ядра
	kernelVersion, err := getKernelVersion()
	if err != nil {
		return fmt.Errorf("failed to get kernel version: %v", err)
	}

	kernelHeadersPath := fmt.Sprintf("/lib/modules/%s/build", kernelVersion)
	if _, err := os.Stat(kernelHeadersPath); os.IsNotExist(err) {
		return fmt.Errorf("kernel headers not found at %s - install linux-headers-%s package",
			kernelHeadersPath, kernelVersion)
	}

	printSuccess("Build requirements check passed")
	return nil
}

// Функция для диагностики сетевых интерфейсов и драйверов
func debugNetworkInterfaces(interfaces []NetworkInterface) {
	printInfo("=== Network Interface Debug Information ===")

	for _, iface := range interfaces {
		if iface.Name == "lo" {
			continue // Skip loopback
		}

		// Получаем дополнительную информацию через разные методы
		ethtoolDriver := getDriverViaEthtool(iface.Name)
		sysfsDriver := getDriverViaSysfs(iface.Name)

		driverType := "UNKNOWN"
		if iface.Driver != "" {
			if isRealtekDriver(iface.Driver) {
				driverType = "REALTEK"
			} else if strings.Contains(strings.ToLower(iface.Driver), "intel") ||
				iface.Driver == "igb" || iface.Driver == "e1000e" ||
				iface.Driver == "ixgbe" || iface.Driver == "i40e" || iface.Driver == "ice" {
				driverType = "INTEL"
			} else {
				driverType = "OTHER"
			}
		}

		printInfo(fmt.Sprintf("Interface %s:", iface.Name))
		printInfo(fmt.Sprintf("  Current Driver: %s [%s]", iface.Driver, driverType))
		printInfo(fmt.Sprintf("  Ethtool Driver: %s", ethtoolDriver))
		printInfo(fmt.Sprintf("  Sysfs Driver: %s", sysfsDriver))
		printInfo(fmt.Sprintf("  MAC: %s", iface.MAC))
		printInfo(fmt.Sprintf("  State: %s", iface.State))
		printInfo(fmt.Sprintf("  IP: %s", iface.IP))
		printInfo("---")
	}

	printInfo("=== End Debug Information ===")
}

// Получение драйвера через ethtool
func getDriverViaEthtool(interfaceName string) string {
	cmd := exec.Command("ethtool", "-i", interfaceName)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("ethtool_error: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "driver:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "not_found"
}

// Получение драйвера через sysfs
func getDriverViaSysfs(interfaceName string) string {
	driverPath := fmt.Sprintf("/sys/class/net/%s/device/driver", interfaceName)
	if link, err := os.Readlink(driverPath); err == nil {
		return filepath.Base(link)
	} else {
		return fmt.Sprintf("sysfs_error: %v", err)
	}
}

// Функция для сохранения скомпилированного драйвера
func saveCompiledDriver(sourceDir, driverDir, driverName, kernelVersion string) (string, error) {
	printInfo("Saving compiled driver...")

	sourcePath := filepath.Join(sourceDir, "pgdrv.ko")
	targetFileName := getDriverFileName(driverName, kernelVersion)
	targetPath := filepath.Join(driverDir, targetFileName)

	// Создаем директорию для драйверов если она не существует
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create driver directory %s: %v", driverDir, err)
	}

	// Копируем файл
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to open source driver %s: %v", sourcePath, err)
	}
	defer sourceFile.Close()

	targetFile, err := os.Create(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to create target driver %s: %v", targetPath, err)
	}
	defer targetFile.Close()

	// Копируем содержимое
	if _, err := sourceFile.WriteTo(targetFile); err != nil {
		return "", fmt.Errorf("failed to copy driver content: %v", err)
	}

	// Устанавливаем права доступа
	if err := os.Chmod(targetPath, 0644); err != nil {
		printWarning(fmt.Sprintf("Failed to set permissions on %s: %v", targetPath, err))
	}

	printSuccess(fmt.Sprintf("Driver saved as: %s", targetPath))
	return targetPath, nil
}

// Driver management functions
func unloadNetworkDriver(driverName string) error {
	if driverName == "" {
		return fmt.Errorf("driver name is empty")
	}

	printInfo(fmt.Sprintf("Unloading driver: %s", driverName))

	// Сначала попробуем выгрузить по имени модуля
	cmd := exec.Command("rmmod", driverName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Если не получилось, попробуем форсированно
		printWarning(fmt.Sprintf("Normal rmmod failed, trying force: %v", err))
		cmd = exec.Command("rmmod", "-f", driverName)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("rmmod failed: %v\nOutput: %s", err, string(output))
		}
	}

	printSuccess(fmt.Sprintf("Driver %s unloaded successfully", driverName))
	return nil
}

func reloadIntelDrivers(drivers []string) {
	for _, driver := range drivers {
		if err := loadNetworkDriver(driver); err != nil {
			printWarning(fmt.Sprintf("Failed to reload driver %s: %v", driver, err))
		} else {
			printSuccess(fmt.Sprintf("Driver %s reloaded successfully", driver))
		}
		time.Sleep(1 * time.Second) // Небольшая пауза между загрузкой драйверов
	}
}

// Функция для загрузки стандартного сетевого драйвера (улучшенная версия)
func loadNetworkDriver(driverName string) error {
	if driverName == "" {
		return fmt.Errorf("driver name is empty")
	}

	printInfo(fmt.Sprintf("Loading driver: %s", driverName))
	cmd := exec.Command("modprobe", driverName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe failed: %v\nOutput: %s", err, string(output))
	}

	// Ждем загрузки драйвера с таймаутом
	if err := waitForDriverLoad(driverName, 10); err != nil {
		printWarning(fmt.Sprintf("Driver load verification timeout: %v", err))
	} else {
		printSuccess(fmt.Sprintf("Driver %s loaded successfully", driverName))
	}

	return nil
}

// Функция для получения версии текущего ядра
func getKernelVersion() (string, error) {
	cmd := exec.Command("uname", "-r")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get kernel version: %v", err)
	}

	version := strings.TrimSpace(string(output))
	return version, nil
}

// Функция для подготовки pgdrv драйвера с проверкой начального состояния
func preparePgdrvDriver(driverDir, originalDriver string, primaryInterface *NetworkInterface) (string, error) {
	printInfo("Checking initial driver state...")

	// Проверяем начальное состояние
	pgdrvLoaded, realtekActive := checkInitialDriverState(primaryInterface)

	printInfo(fmt.Sprintf("Initial state: pgdrv loaded=%t, realtek active=%t", pgdrvLoaded, realtekActive))

	if pgdrvLoaded && !realtekActive {
		// Случай 1: pgdrv уже загружен и нет конфликтующих Realtek драйверов
		printSuccess("pgdrv already loaded and no conflicting Realtek drivers - ready for flashing")
		return "pgdrv_already_loaded", nil
	}

	if pgdrvLoaded && realtekActive {
		// Случай 2: pgdrv загружен, но есть активный Realtek драйвер - конфликт
		printWarning("pgdrv loaded but Realtek driver also active - resolving conflict")

		// Выгружаем оба драйвера
		if err := unloadPgdrvDriver(); err != nil {
			printError(fmt.Sprintf("Failed to unload pgdrv: %v", err))
		}
		if err := unloadNetworkDriver(primaryInterface.Driver); err != nil {
			printError(fmt.Sprintf("Failed to unload Realtek driver %s: %v", primaryInterface.Driver, err))
		}

		printInfo("Both drivers unloaded, proceeding to load clean pgdrv...")
	} else if !pgdrvLoaded && realtekActive {
		// Случай 3: Стандартная ситуация - pgdrv не загружен, Realtek активен
		printInfo("Standard case: unloading Realtek driver to load pgdrv")
		if err := unloadNetworkDriver(primaryInterface.Driver); err != nil {
			return "", fmt.Errorf("failed to unload Realtek driver %s: %v", primaryInterface.Driver, err)
		}
	} else {
		// Случай 4: Ни один драйвер не загружен
		printInfo("No conflicting drivers found, proceeding to load pgdrv")
	}

	// Загружаем pgdrv драйвер
	return loadFlashingDriver(driverDir, originalDriver)
}

// Заменяем функцию compileFlashingDriver на реальную реализацию
func compileFlashingDriver(driverDir string, originalDriver string) (string, error) {
	printInfo("Compiling rtnicpg driver from sources...")

	// Проверяем наличие необходимых инструментов для компиляции
	if err := checkBuildRequirements(); err != nil {
		return "", fmt.Errorf("build requirements not met: %v", err)
	}

	// Ищем исходники rtnicpg
	sourceDir, found := checkRtnicpgSources(driverDir)
	if !found {
		return "", fmt.Errorf("rtnicpg source directory not found in %s", driverDir)
	}

	// Сохраняем текущую директорию
	originalDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %v", err)
	}

	// Переходим в директорию с исходниками
	if err := os.Chdir(sourceDir); err != nil {
		return "", fmt.Errorf("failed to change to source directory %s: %v", sourceDir, err)
	}

	// Восстанавливаем директорию при выходе
	defer func() {
		os.Chdir(originalDir)
	}()

	// Очищаем предыдущие артефакты сборки
	printInfo("Cleaning previous build artifacts...")
	cleanCmd := exec.Command("make", "clean")
	cleanCmd.Dir = sourceDir
	if output, err := cleanCmd.CombinedOutput(); err != nil {
		printWarning(fmt.Sprintf("Clean failed (non-critical): %v\nOutput: %s", err, string(output)))
	}

	// Получаем версию ядра для переменной окружения
	kernelVersion, err := getKernelVersion()
	if err != nil {
		return "", fmt.Errorf("failed to get kernel version: %v", err)
	}

	// Компилируем драйвер
	printInfo("Building driver module...")
	buildCmd := exec.Command("make", "all")
	buildCmd.Dir = sourceDir
	buildCmd.Env = append(os.Environ(),
		"KERNELDIR=/lib/modules/"+kernelVersion+"/build",
	)

	output, err := buildCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("compilation failed: %v\nOutput: %s", err, string(output))
	}

	// Проверяем, что файл pgdrv.ko был создан
	compiledDriverPath := filepath.Join(sourceDir, "pgdrv.ko")
	if _, err := os.Stat(compiledDriverPath); os.IsNotExist(err) {
		return "", fmt.Errorf("compilation succeeded but pgdrv.ko not found at %s", compiledDriverPath)
	}

	printSuccess("Driver compilation completed successfully")

	// Сохраняем драйвер в папку драйверов
	savedDriverPath, err := saveCompiledDriver(sourceDir, driverDir, originalDriver, kernelVersion)
	if err != nil {
		return "", fmt.Errorf("failed to save compiled driver: %v", err)
	}

	return savedDriverPath, nil
}

// Функция для определения является ли драйвер Realtek'овским
func isRealtekDriver(driverName string) bool {
	realtekDrivers := []string{
		"r8169",   // Realtek RTL8169/8110 PCI Gigabit Ethernet
		"r8168",   // Realtek RTL8168 PCI Express Gigabit Ethernet
		"rtl8169", // Alternative name
		"rtl8168", // Alternative name
		"r8125",   // Realtek RTL8125 2.5Gigabit Ethernet
		"rtl8125", // Alternative name
		"8139too", // Realtek RTL-8139 (legacy)
		"8139cp",  // Realtek RTL-8139C+ (legacy)
		"rtl8139", // Alternative name (legacy)
		"r8152",   // Realtek RTL8152/RTL8153 USB Ethernet
		"rtl8152", // Alternative name
	}

	driverLower := strings.ToLower(driverName)
	for _, realtekDriver := range realtekDrivers {
		if driverLower == realtekDriver {
			return true
		}
	}
	return false
}

// Функция для поиска Realtek интерфейса среди доступных (обновленная с диагностикой)
func findRealtekInterface(interfaces []NetworkInterface) *NetworkInterface {
	printInfo("Searching for Realtek interfaces...")

	var realtekInterfaces []*NetworkInterface

	// Собираем все Realtek интерфейсы
	for i := range interfaces {
		if interfaces[i].Driver != "" && isRealtekDriver(interfaces[i].Driver) {
			realtekInterfaces = append(realtekInterfaces, &interfaces[i])
			printInfo(fmt.Sprintf("Found Realtek interface: %s (Driver: %s, State: %s, IP: %s)",
				interfaces[i].Name, interfaces[i].Driver, interfaces[i].State, interfaces[i].IP))
		}
	}

	if len(realtekInterfaces) == 0 {
		printWarning("No Realtek interfaces found by driver name")
		return nil
	}

	// Сначала ищем активный Realtek интерфейс с IP
	for _, iface := range realtekInterfaces {
		if iface.IP != "" && iface.State == "UP" {
			printSuccess(fmt.Sprintf("Selected active Realtek interface with IP: %s", iface.Name))
			return iface
		}
	}

	// Если не нашли активный с IP, ищем активный без IP
	for _, iface := range realtekInterfaces {
		if iface.State == "UP" {
			printInfo(fmt.Sprintf("Selected active Realtek interface (no IP): %s", iface.Name))
			return iface
		}
	}

	// Если не нашли активный, берем первый найденный
	printWarning(fmt.Sprintf("Selected inactive Realtek interface: %s", realtekInterfaces[0].Name))
	return realtekInterfaces[0]
}

// Flashing execution functions
func executeRtnicFlashing(targetMAC string) error {
	// Remove colons from MAC for rtnic
	macWithoutColons := strings.ReplaceAll(targetMAC, ":", "")

	printInfo(fmt.Sprintf("Executing rtnic flashing for MAC: %s", targetMAC))

	// Execute rtnic with required arguments
	cmd := exec.Command("rtnic", "/efuse", "/nicmac", "/nodeid", macWithoutColons)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("rtnic command failed: %v\nOutput: %s", err, string(output))
	}

	// Check if output indicates success
	outputStr := string(output)
	if strings.Contains(strings.ToLower(outputStr), "error") || strings.Contains(strings.ToLower(outputStr), "fail") {
		return fmt.Errorf("rtnic reported error: %s", outputStr)
	}

	printSuccess("rtnic flashing command completed successfully")
	return nil
}

func restoreIPAddress(interfaceName, ipAddress string) error {
	if interfaceName == "" || ipAddress == "" {
		return fmt.Errorf("interface name or IP address is empty")
	}

	printInfo(fmt.Sprintf("Restoring IP %s to interface %s", ipAddress, interfaceName))

	// First ensure interface is up
	cmd := exec.Command("ip", "link", "set", interfaceName, "up")
	cmd.Run()

	time.Sleep(1 * time.Second)

	// Assign IP address (assuming /24 subnet)
	cmd = exec.Command("ip", "addr", "add", ipAddress+"/24", "dev", interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// IP might already be assigned, check if it's actually there
		checkCmd := exec.Command("ip", "addr", "show", interfaceName)
		checkOutput, _ := checkCmd.Output()
		if strings.Contains(string(checkOutput), ipAddress) {
			printSuccess(fmt.Sprintf("IP %s already assigned to %s", ipAddress, interfaceName))
			return nil
		}
		return fmt.Errorf("failed to assign IP: %v\nOutput: %s", err, string(output))
	}

	printSuccess(fmt.Sprintf("IP %s restored to interface %s", ipAddress, interfaceName))
	return nil
}

func runFlashing(config FlashConfig, flashData *FlashData, systemConfig SystemConfig) ([]FlashResult, bool) {
	var results []FlashResult
	var serialNumberChanged bool = false

	if !config.Enabled {
		return results, false
	}

	fmt.Println(strings.Repeat("-", 80))

	// Логируем то, что будем прошивать
	printInfo("Flashing operations summary:")
	if flashData.SystemSerial != "" {
		printInfo(fmt.Sprintf("  System Serial -> %s", flashData.SystemSerial))
	}
	if flashData.IOBoard != "" {
		printInfo(fmt.Sprintf("  IO Board      -> %s", flashData.IOBoard))
	}
	if flashData.MAC != "" {
		printInfo(fmt.Sprintf("  MAC Address   -> %s", flashData.MAC))
	}

	for _, operation := range config.Operations {
		result := FlashResult{
			Operation: operation,
			Status:    "PASSED",
		}

		startTime := time.Now()

		switch operation {
		case "mac":
			printInfo(fmt.Sprintf("Flashing MAC address: %s", flashData.MAC))
			err := flashMAC(config, systemConfig, flashData.MAC)
			if err != nil {
				result.Status = "FAILED"
				result.Details = fmt.Sprintf("MAC flash failed: %v", err)
			}

		case "efi":
			printInfo("Updating EFI variables")
			efiChanged, efiSerialChanged, err := updateEFIVariables(systemConfig, flashData)
			if err != nil {
				result.Status = "FAILED"
				result.Details = fmt.Sprintf("EFI update failed: %v", err)
			} else if !efiChanged {
				result.Status = "SKIPPED"
				result.Details = "All EFI variables already have correct values"
			}

			if efiSerialChanged {
				serialNumberChanged = true
			}

		case "fru":
			printInfo("Flashing FRU chip...")
			if flashData.SystemSerial != "" {
				fruSerialChanged, err := flashFRU(systemConfig, flashData.SystemSerial)
				if err != nil {
					result.Status = "FAILED"
					result.Details = fmt.Sprintf("FRU flash failed: %v", err)
				} else if !fruSerialChanged {
					result.Status = "SKIPPED"
					result.Details = "FRU already contains target serial number"
				} else {
					printSuccess("FRU chip flashed successfully")
					serialNumberChanged = true
				}
			} else {
				result.Status = "FAILED"
				result.Details = "No system serial number provided for FRU flashing"
			}
		}

		result.Duration = time.Since(startTime)
		results = append(results, result)

		outputManager.PrintResult(time.Now(), operation, result.Status, result.Duration, result.Details)
	}

	return results, serialNumberChanged
}

func validateEFISystem() error {
	// Check if system supports EFI variables
	if _, err := os.Stat("/sys/firmware/efi/efivars"); os.IsNotExist(err) {
		return fmt.Errorf("EFI variables not supported on this system (efivars not found)")
	}

	// Try to create UEFI context
	ctx := efivario.NewDefaultContext()
	if ctx == nil {
		return fmt.Errorf("failed to create UEFI context")
	}

	printSuccess("EFI system validation passed")
	return nil
}

func setEFIVariable(guidPrefix, varName, value string) error {
	printInfo(fmt.Sprintf("Setting EFI variable %q to: %q", varName, value))

	// Проверка имени и содержимого переменной
	if varName == "" || len(varName) > 1024 {
		return fmt.Errorf("invalid variable name")
	}
	if len(value) == 0 || len(value) > 1024 {
		return fmt.Errorf("invalid value length")
	}

	// Парсим GUID
	varGUID, err := efiguid.FromString(guidPrefix)
	if err != nil {
		return fmt.Errorf("invalid GUID format '%s': %v", guidPrefix, err)
	}

	ctx := efivario.NewDefaultContext()
	if ctx == nil {
		return fmt.Errorf("failed to create UEFI context")
	}

	const (
		EFI_VARIABLE_NON_VOLATILE       = 0x00000001
		EFI_VARIABLE_BOOTSERVICE_ACCESS = 0x00000002
		EFI_VARIABLE_RUNTIME_ACCESS     = 0x00000004
	)

	attributes := efivario.Attributes(
		EFI_VARIABLE_NON_VOLATILE |
			EFI_VARIABLE_BOOTSERVICE_ACCESS |
			EFI_VARIABLE_RUNTIME_ACCESS,
	)

	data := []byte(value)

	fmt.Printf("→ Writing EFI var: name=%q, guid=%s, len=%d, attrs=0x%X\n",
		varName, varGUID.String(), len(data), uint32(attributes))

	fmt.Printf("→ EFI var: data=%s\n",
		data)

	err = ctx.Set(varName, varGUID, attributes, data)
	if err != nil {
		if strings.Contains(err.Error(), "invalid argument") {
			printError("Hint: check if efivarfs is mounted as rw and that the data format is valid")
			printError("Some firmware may also reject certain variable names or GUIDs")
		}
		return fmt.Errorf("failed to set EFI variable %s: %v", varName, err)
	}

	// Проверка записи
	readBuf := make([]byte, 1024)
	readAttrs, n, err := ctx.Get(varName, varGUID, readBuf)
	if err != nil {
		printWarning(fmt.Sprintf("Variable %s was set but cannot be read back: %v", varName, err))
	} else {
		readData := readBuf[:n]
		fmt.Printf("→ Read back EFI var: len=%d (written=%d)\n", n, len(data))
		fmt.Printf("→ Attributes: 0x%X\n", uint32(readAttrs))

		if bytes.Equal(readData, data) {
			printSuccess(fmt.Sprintf("EFI variable %s verified value: %q (attrs: 0x%x)", varName, readData, readAttrs))
		} else {
			printWarning(fmt.Sprintf(
				"EFI variable %s value mismatch:\n  expected (len %d): %q (hex: %X)\n       got (len %d): %q (hex: %X)",
				varName, len(data), data, data, len(readData), readData, readData,
			))
		}
	}

	return nil
}

func testServerConnection(config LogConfig) error {
	if !config.SendLogs || config.Server == "" {
		return nil
	}

	// Parse server (user@host format)
	serverParts := strings.Split(config.Server, "@")
	if len(serverParts) != 2 {
		return fmt.Errorf("invalid server format, expected user@host: %s", config.Server)
	}

	user := serverParts[0]
	host := serverParts[1]
	serverAddr := fmt.Sprintf("%s@%s", user, host)

	printInfo(fmt.Sprintf("Testing connection to server: %s", serverAddr))

	// Test SSH connection
	testCmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		serverAddr,
		"echo 'Connection test successful'")

	if output, err := testCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("server connection test failed: %v\nOutput: %s", err, string(output))
	}

	printSuccess("Server connection test passed")
	return nil
}

func sendLogToServer(log SessionLog, config LogConfig) error {
	if !config.SendLogs || config.Server == "" {
		return nil
	}

	printInfo(fmt.Sprintf("Sending log to server: %s", config.Server))

	// Marshal to YAML
	data, err := yaml.Marshal(log)
	if err != nil {
		return fmt.Errorf("failed to marshal log: %v", err)
	}

	// Create temporary file
	tmpFile, err := os.CreateTemp("", "system_validator_*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(data)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	tmpFile.Close()

	// Generate remote filename with state
	timestamp := log.Timestamp.Format("20060102_150405")
	remoteFile := fmt.Sprintf("%s_%s_%s_%s.yaml", log.System.Product, log.System.MBSerial, timestamp, log.State)

	// Build remote directory path
	remoteDirParts := []string{}
	if config.ServerDir != "" {
		remoteDirParts = append(remoteDirParts, config.ServerDir)
	}
	if log.System.Product != "" {
		remoteDirParts = append(remoteDirParts, log.System.Product)
	}
	if config.OpName != "" {
		remoteDirParts = append(remoteDirParts, config.OpName)
	}

	var remoteDir string
	if len(remoteDirParts) > 0 {
		remoteDir = strings.Join(remoteDirParts, "/")
	} else {
		remoteDir = "."
	}

	// Parse server (user@host format)
	serverParts := strings.Split(config.Server, "@")
	if len(serverParts) != 2 {
		return fmt.Errorf("invalid server format, expected user@host: %s", config.Server)
	}

	user := serverParts[0]
	host := serverParts[1]
	serverAddr := fmt.Sprintf("%s@%s", user, host)

	fmt.Printf("Remote: %s:%s/%s\n", serverAddr, remoteDir, remoteFile)

	// Step 1: Create remote directories if they don't exist
	if remoteDir != "." {
		createCmd := fmt.Sprintf("mkdir -p \"%s\"", remoteDir)
		cmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			serverAddr, createCmd)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create remote directory: %v", err)
		}
	}

	// Step 2: Upload file
	remoteFullPath := fmt.Sprintf("%s/%s", remoteDir, remoteFile)
	scpTarget := fmt.Sprintf("%s:%s", serverAddr, remoteFullPath)

	cmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		tmpFile.Name(), scpTarget)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to upload file: %v\nOutput: %s", err, string(output))
	}

	printSuccess("Log successfully sent to server")
	return nil
}

// getCurrentFRUSerial читает текущий серийный номер из FRU чипа
func getCurrentFRUSerial() (string, error) {
	cmd := exec.Command("ipmitool", "fru", "print", "0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Board Serial") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				serial := strings.TrimSpace(parts[1])
				if serial == "" || serial == "Not Specified" || serial == "Unknown" {
					return "", fmt.Errorf("no valid serial number found in FRU")
				}
				return serial, nil
			}
		}
	}

	return "", fmt.Errorf("Board Serial field not found in FRU data")
}

func checkFRUStatus() (*FRUStatus, error) {
	printInfo("Checking FRU chip status...")

	status := &FRUStatus{}

	// Try to read FRU data using ipmitool
	cmd := exec.Command("ipmitool", "fru", "print", "0")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	if err != nil {
		printWarning(fmt.Sprintf("FRU read returned error: %v", err))
		status.CanRead = false
		status.ErrorMessage = err.Error()

		// Check specific error patterns that indicate FRU needs initialization
		outputLower := strings.ToLower(outputStr)

		if strings.Contains(outputLower, "unknown fru header version") {
			status.IsEmpty = true
			status.HasBadSum = true // Corrupted header also needs blank flash
			printWarning("FRU has corrupted header (Unknown FRU header version) - needs initialization")
		} else if strings.Contains(outputLower, "no fru data") ||
			strings.Contains(outputLower, "invalid") ||
			strings.Contains(outputLower, "empty") {
			status.IsEmpty = true
			printWarning("FRU appears to be empty")
		} else if strings.Contains(outputLower, "checksum") ||
			strings.Contains(outputLower, "badchecksum") {
			status.HasBadSum = true
			printWarning("FRU has bad checksum")
		} else if strings.Contains(outputLower, "fru read failed") ||
			strings.Contains(outputLower, "fru data checksum") {
			status.HasBadSum = true
			printWarning("FRU data corruption detected")
		} else {
			// For any other FRU read error, assume it needs reinitialization
			status.IsEmpty = true
			status.HasBadSum = true
			printWarning(fmt.Sprintf("FRU read failed with unknown error - assuming corruption: %s", outputStr))
		}
	} else {
		status.CanRead = true
		status.IsPresent = true

		// Check if FRU has actual valid data
		if strings.Contains(outputStr, "Board Mfg") ||
			strings.Contains(outputStr, "Board Product") ||
			strings.Contains(outputStr, "Board Serial") {
			printSuccess("FRU contains valid data")
		} else {
			status.IsEmpty = true
			printInfo("FRU is readable but appears empty")
		}
	}

	// Summary of status
	if status.IsEmpty && status.HasBadSum {
		printInfo("FRU Status: Corrupted/Empty - requires blank initialization")
	} else if status.IsEmpty {
		printInfo("FRU Status: Empty - requires initialization")
	} else if status.HasBadSum {
		printInfo("FRU Status: Bad checksum - requires reinitialization")
	} else if status.CanRead {
		printInfo("FRU Status: Valid data present")
	}

	return status, nil
}

func createFRUBlankFile() (string, error) {
	printInfo("Creating blank FRU file (2048 null bytes - equivalent to 'dd if=/dev/zero bs=2048 count=1')...")

	tmpFile, err := os.CreateTemp("", "fru_blank_*.bin")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	// Write 2048 null bytes (same as dd if=/dev/zero of=file bs=2048 count=1)
	nullData := make([]byte, 2048)
	bytesWritten, err := tmpFile.Write(nullData)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to write blank data: %v", err)
	}

	if bytesWritten != 2048 {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("wrote %d bytes, expected 2048", bytesWritten)
	}

	printSuccess(fmt.Sprintf("Blank FRU file created: %s (%d bytes)", tmpFile.Name(), bytesWritten))
	return tmpFile.Name(), nil
}

func flashFRUFile(filename string) error {
	printInfo(fmt.Sprintf("Flashing FRU file: %s", filename))

	// Use ipmitool to write FRU file
	cmd := exec.Command("ipmitool", "fru", "write", "0", filename)
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	if err != nil {
		return fmt.Errorf("FRU flash failed: %v\nOutput: %s", err, outputStr)
	}

	// Check for success indicators in output
	if strings.Contains(strings.ToLower(outputStr), "success") ||
		strings.Contains(strings.ToLower(outputStr), "written") ||
		len(outputStr) == 0 { // Sometimes ipmitool outputs nothing on success
		printSuccess("FRU file flashed successfully")
		return nil
	}

	// Check for error indicators
	if strings.Contains(strings.ToLower(outputStr), "error") ||
		strings.Contains(strings.ToLower(outputStr), "fail") {
		return fmt.Errorf("FRU flash reported error: %s", outputStr)
	}

	// If no clear indicators, assume success (some ipmitool versions are quiet)
	printSuccess("FRU flash command completed")
	return nil
}

func generateFRUFile(systemConfig SystemConfig, serialNumber string) (string, error) {
	printInfo("Generating FRU file with frugen...")

	// Create temporary file for FRU output
	tmpFile, err := os.CreateTemp("", "fru_generated_*.bin")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	tmpFile.Close() // Close it so frugen can write to it

	// Prepare frugen command
	manufacturer := systemConfig.Manufacturer
	if manufacturer == "" {
		manufacturer = "Unknown" // fallback
	}

	product := systemConfig.Product
	if product == "" {
		product = "Unknown" // fallback
	}

	cmd := exec.Command("frugen",
		"--board-mfg", manufacturer,
		"--board-pname", product,
		"--board-serial", serialNumber,
		"--ascii",
		tmpFile.Name())

	printInfo(fmt.Sprintf("Executing: frugen --board-mfg \"%s\" --board-pname \"%s\" --board-serial \"%s\" --ascii %s",
		manufacturer, product, serialNumber, tmpFile.Name()))

	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	if err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("frugen failed: %v\nOutput: %s", err, outputStr)
	}

	// Check if file was actually created
	if _, err := os.Stat(tmpFile.Name()); os.IsNotExist(err) {
		return "", fmt.Errorf("frugen did not create output file")
	}

	printSuccess(fmt.Sprintf("FRU file generated: %s", tmpFile.Name()))
	if outputStr != "" {
		printInfo(fmt.Sprintf("frugen output: %s", outputStr))
	}

	return tmpFile.Name(), nil
}

func verifyFRUData(expectedManufacturer, expectedProduct, expectedSerial string) error {
	printInfo("Verifying FRU data...")

	// Wait a moment for FRU to be readable after flashing
	time.Sleep(2 * time.Second)

	cmd := exec.Command("ipmitool", "fru", "print", "0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to read FRU for verification: %v", err)
	}

	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")

	var foundMfg, foundProduct, foundSerial string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Board Mfg") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				foundMfg = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "Board Product") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				foundProduct = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "Board Serial") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				foundSerial = strings.TrimSpace(parts[1])
			}
		}
	}

	// Check each field
	var errors []string

	if foundMfg != expectedManufacturer {
		errors = append(errors, fmt.Sprintf("Manufacturer mismatch: expected '%s', found '%s'", expectedManufacturer, foundMfg))
	}

	if foundProduct != expectedProduct {
		errors = append(errors, fmt.Sprintf("Product mismatch: expected '%s', found '%s'", expectedProduct, foundProduct))
	}

	if foundSerial != expectedSerial {
		errors = append(errors, fmt.Sprintf("Serial mismatch: expected '%s', found '%s'", expectedSerial, foundSerial))
	}

	if len(errors) > 0 {
		return fmt.Errorf("FRU verification failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	printSuccess("FRU verification passed")
	printInfo(fmt.Sprintf("  Manufacturer: %s", foundMfg))
	printInfo(fmt.Sprintf("  Product: %s", foundProduct))
	printInfo(fmt.Sprintf("  Serial: %s", foundSerial))

	return nil
}

func askFRURetryAction(message string) string {
	fmt.Printf("\n%s=== FRU FLASHING ERROR ===%s\n", ColorRed, ColorReset)
	fmt.Printf("%s\n", message)
	fmt.Println("Choose action:")
	fmt.Printf("  %s[Y]%s Yes - Retry FRU flashing (default)\n", ColorGreen, ColorReset)
	fmt.Printf("  %s[A]%s Abort - Stop FRU flashing and continue program\n", ColorYellow, ColorReset)
	fmt.Printf("  %s[S]%s Skip - Skip FRU flashing by operator decision\n", ColorBlue, ColorReset)
	fmt.Printf("Choice [Y/a/s]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "RETRY" // default on error
	}

	choice := strings.ToUpper(strings.TrimSpace(input))
	if choice == "" {
		choice = "Y" // default
	}

	switch choice {
	case "Y", "YES":
		return "RETRY"
	case "A", "ABORT":
		return "ABORT"
	case "S", "SKIP":
		return "SKIP"
	default:
		fmt.Printf("Invalid choice '%s', defaulting to retry.\n", choice)
		return "RETRY"
	}
}

// Модифицированная функция updateEFIVariables с возвращением информации об изменениях серийного номера
func updateEFIVariables(config SystemConfig, flashData *FlashData) (bool, bool, error) {
	printInfo("Updating EFI variables...")

	// Validate EFI system before proceeding
	if err := validateEFISystem(); err != nil {
		return false, false, fmt.Errorf("EFI system validation failed: %v", err)
	}

	anyChanges := false
	serialChanged := false

	// Update system serial number EFI variable
	if flashData.SystemSerial != "" && config.EfiSnName != "" {
		// Проверяем существующее значение
		existingSerial, err := getEFIVariable(config.GuidPrefix, config.EfiSnName)
		if err == nil && existingSerial == flashData.SystemSerial {
			printInfo(fmt.Sprintf("EFI variable %s already contains target value: %s - skipping",
				config.EfiSnName, flashData.SystemSerial))
		} else {
			if err == nil {
				printInfo(fmt.Sprintf("EFI variable %s current value: %s, updating to: %s",
					config.EfiSnName, existingSerial, flashData.SystemSerial))
			} else {
				printInfo(fmt.Sprintf("EFI variable %s does not exist, creating with value: %s",
					config.EfiSnName, flashData.SystemSerial))
			}

			err := setEFIVariable(config.GuidPrefix, config.EfiSnName, flashData.SystemSerial)
			if err != nil {
				return false, false, fmt.Errorf("failed to set serial EFI variable: %v", err)
			}
			anyChanges = true
			serialChanged = true // Серийный номер изменился!
		}
	}

	// Update MAC address EFI variable
	if flashData.MAC != "" && config.EfiMacName != "" {
		// Convert MAC to the format expected by EFI (remove colons, uppercase)
		hexMAC := strings.ReplaceAll(strings.ToUpper(flashData.MAC), ":", "")

		// Проверяем существующее значение
		existingMAC, err := getEFIVariable(config.GuidPrefix, config.EfiMacName)
		if err == nil && existingMAC == hexMAC {
			printInfo(fmt.Sprintf("EFI variable %s already contains target value: %s (MAC: %s) - skipping",
				config.EfiMacName, hexMAC, flashData.MAC))
		} else {
			if err == nil {
				printInfo(fmt.Sprintf("EFI variable %s current value: %s, updating to: %s (MAC: %s)",
					config.EfiMacName, existingMAC, hexMAC, flashData.MAC))
			} else {
				printInfo(fmt.Sprintf("EFI variable %s does not exist, creating with value: %s (MAC: %s)",
					config.EfiMacName, hexMAC, flashData.MAC))
			}

			err := setEFIVariable(config.GuidPrefix, config.EfiMacName, hexMAC)
			if err != nil {
				return false, false, fmt.Errorf("failed to set MAC EFI variable: %v", err)
			}
			anyChanges = true
			// MAC не требует перезагрузки, serialChanged остается прежним
		}
	}

	if anyChanges {
		printSuccess("EFI variables updated successfully")
	} else {
		printSuccess("All EFI variables already have correct values - no changes needed")
	}

	return anyChanges, serialChanged, nil
}

// Модифицированная функция flashFRU с возвращением информации об изменении серийного номера
func flashFRU(systemConfig SystemConfig, serialNumber string) (bool, error) {
	// Проверяем существующий серийный номер в FRU (НЕ в dmidecode!)
	currentSerial, err := getCurrentFRUSerial()
	if err == nil && currentSerial == serialNumber {
		printInfo(fmt.Sprintf("FRU already contains target serial number: %s - skipping FRU flashing", serialNumber))
		return false, nil // Серийный номер не изменился
	}

	if err == nil {
		printInfo(fmt.Sprintf("Current FRU serial: %s, updating to: %s", currentSerial, serialNumber))
	} else {
		printInfo(fmt.Sprintf("Could not read current FRU serial (%v), proceeding with FRU flash to: %s", err, serialNumber))
	}

	printSubHeader("FRU CHIP FLASHING", fmt.Sprintf("Target Serial: %s | Manufacturer: %s", serialNumber, systemConfig.Manufacturer))

	// Step 1: Check current FRU status
	status, err := checkFRUStatus()
	if err != nil {
		return false, fmt.Errorf("failed to check FRU status: %v", err)
	}

	// Step 2: If FRU has bad checksum or is empty, flash blank first
	needsBlankFlash := status.HasBadSum || status.IsEmpty || !status.CanRead

	if needsBlankFlash {
		if status.HasBadSum && status.IsEmpty {
			printInfo("FRU has corrupted header - initializing with blank data...")
		} else if status.HasBadSum {
			printInfo("FRU has bad checksum - clearing with blank data...")
		} else if status.IsEmpty {
			printInfo("FRU is empty - initializing with blank data...")
		} else {
			printInfo("FRU is unreadable - clearing with blank data...")
		}

		blankFile, err := createFRUBlankFile()
		if err != nil {
			return false, fmt.Errorf("failed to create blank FRU file: %v", err)
		}
		defer os.Remove(blankFile)

		printInfo("Flashing 2048-byte null file to clear FRU...")
		if err := flashFRUFile(blankFile); err != nil {
			return false, fmt.Errorf("failed to flash blank FRU: %v", err)
		}

		printSuccess("Blank FRU flash completed")

		// Wait for FRU to be ready after blank flash
		printInfo("Waiting for FRU to stabilize...")
		time.Sleep(3 * time.Second)
	}

	// Step 3: Generate and flash FRU with retries
	attempts := 0
	maxAttempts := 3
	var lastError error

	for attempts < maxAttempts {
		attempts++
		printInfo(fmt.Sprintf("FRU generation and flashing attempt %d/%d...", attempts, maxAttempts))

		// Generate FRU file
		fruFile, err := generateFRUFile(systemConfig, serialNumber)
		if err != nil {
			lastError = fmt.Errorf("FRU generation failed: %v", err)
			printError(lastError.Error())
		} else {
			defer os.Remove(fruFile)

			// Flash FRU file
			if err := flashFRUFile(fruFile); err != nil {
				lastError = fmt.Errorf("FRU flashing failed: %v", err)
				printError(lastError.Error())
			} else {
				// Verify FRU data
				if err := verifyFRUData(systemConfig.Manufacturer, systemConfig.Product, serialNumber); err != nil {
					lastError = fmt.Errorf("FRU verification failed: %v", err)
					printError(lastError.Error())
				} else {
					// Success!
					printSuccess("FRU flashing completed successfully")
					return true, nil // Серийный номер был изменен!
				}
			}
		}

		// If we failed and have more attempts, ask user what to do
		if attempts < maxAttempts {
			action := askFRURetryAction(fmt.Sprintf("FRU flashing failed (attempt %d/%d): %v", attempts, maxAttempts, lastError))
			switch action {
			case "SKIP":
				printWarning("FRU flashing skipped by operator")
				return false, nil
			case "ABORT":
				return false, fmt.Errorf("FRU flashing aborted by operator")
			case "RETRY":
				printInfo("Retrying FRU flashing...")
				continue
			}
		}
	}

	// All attempts failed
	return false, fmt.Errorf("FRU flashing failed after %d attempts: %v", maxAttempts, lastError)
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
			printDebug(fmt.Sprintf("Found archiso boot mount: %s", bootMntSource))

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

	printDebug(fmt.Sprintf("All disks: %v", disks))
	printDebug(fmt.Sprintf("Boot device: %s", bootDev))

	// Check if we're running from ArchISO/live environment

	// Check what device /run/archiso/bootmnt is mounted on (if we're in a live environment)
	var archisoDev string
	if bootMntSource, err := runCommand("findmnt", "/run/archiso/bootmnt", "-o", "SOURCE", "-n"); err == nil && bootMntSource != "" {
		bootMntSource = strings.TrimSpace(bootMntSource)
		printDebug(fmt.Sprintf("Found archiso boot mount: %s", bootMntSource))

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
		printDebug(fmt.Sprintf("Extracted archiso device: %s", archisoDev))
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

		printDebug(fmt.Sprintf("Checking disk: %s for partitions (boot device: %v)", dev, isBootDevice))

		// Get all partitions for this disk
		output, err := runCommand("lsblk", "-nlo", "NAME", dev)
		if err != nil {
			printDebug(fmt.Sprintf("Error listing partitions for %s: %v", dev, err))
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
			printDebug(fmt.Sprintf("Checking partition: %s", partPath))

			// Skip if it's the same as disk device
			if partPath == dev {
				printDebug(fmt.Sprintf("Skipping partition %s as it's the same as disk device", partPath))
				continue
			}

			if isEfiPartition(partPath) {
				printDebug(fmt.Sprintf("Found EFI partition: %s on disk: %s", partPath, dev))

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
			printDebug(fmt.Sprintf("Multiple EFI partitions found on boot device. Using the first one."))
			for i, part := range bootDevEfiPartitions {
				printDebug(fmt.Sprintf("Boot device EFI partition %d: disk=%s, partition=%s", i+1, part.disk, part.partition))
			}
		}
		printDebug(fmt.Sprintf("Selected EFI partition on boot device: %s", bootDevEfiPartitions[0].partition))
		return bootDevEfiPartitions[0].disk, bootDevEfiPartitions[0].partition, nil
	}

	// If no EFI partitions found on boot device, fall back to other disks
	if len(otherEfiPartitions) > 0 {
		if len(otherEfiPartitions) > 1 {
			printDebug(fmt.Sprintf("Multiple EFI partitions found on other devices. Using the first one."))
			for i, part := range otherEfiPartitions {
				printDebug(fmt.Sprintf("Other device EFI partition %d: disk=%s, partition=%s", i+1, part.disk, part.partition))
			}
		}
		printDebug(fmt.Sprintf("Selected EFI partition on non-boot device: %s", otherEfiPartitions[0].partition))
		return otherEfiPartitions[0].disk, otherEfiPartitions[0].partition, nil
	}

	// If we get here, no EFI partition was found
	return "", "", errors.New("no EFI partition found on any disk")
}

// getEFIVariable читает существующую EFI переменную
func getEFIVariable(guidPrefix, varName string) (string, error) {
	// Парсим GUID
	varGUID, err := efiguid.FromString(guidPrefix)
	if err != nil {
		return "", fmt.Errorf("invalid GUID format '%s': %v", guidPrefix, err)
	}

	ctx := efivario.NewDefaultContext()
	if ctx == nil {
		return "", fmt.Errorf("failed to create UEFI context")
	}

	// Читаем переменную
	readBuf := make([]byte, 1024)
	_, n, err := ctx.Get(varName, varGUID, readBuf)
	if err != nil {
		return "", err // Переменная не существует или не читается
	}

	readData := readBuf[:n]
	return string(readData), nil
}

// bootctl mounts external EFI partition, copies contents of efishell directory (ctefi)
// and sets one-time boot entry (via setOneTimeBoot). Do not change this function!
func bootctl() error {
	// Determine boot device
	bootDev, err := findBootDevice()
	if err != nil {
		return fmt.Errorf("Could not determine boot device: %v", err)
	}

	printDebug(fmt.Sprintf("Detected boot device: %s", bootDev))

	// Find external EFI partition
	targetDevice, targetEfi, err := findExternalEfiPartition(bootDev)
	if err != nil || targetDevice == "" || targetEfi == "" {
		return errors.New("No external EFI partition found")
	}

	// Additional check to ensure targetEfi is a partition, not the whole disk
	if targetEfi == targetDevice {
		return fmt.Errorf("targetEfi cannot be the same as targetDevice: %s", targetEfi)
	}

	printDebug("targetDevice: " + targetDevice)
	printDebug("targetEFI: " + targetEfi)

	// No need to mount and copy files, as all necessary information is in EFI variables
	printDebug("Using EFI variables instead of copying files to EFI partition")

	// Call setOneTimeBoot function to create new entry and set BootNext
	if err := setOneTimeBoot(targetDevice, targetEfi); err != nil {
		return fmt.Errorf("setOneTimeBoot error: %v", err)
	}

	if err = runCommandNoOutput("bootctl", "set-oneshot", "03-efishell.conf"); err != nil {
		printError("Failed to set one-time boot entry: " + err.Error())
		os.Exit(1)
	} else {
		printDebug("One-time boot entry set successfully.")
	}

	return nil
}

// setOneTimeBoot creates a new one-time boot entry and sets BootNext
func setOneTimeBoot(targetDevice, targetEfi string) error {
	printDebug(fmt.Sprintf("setOneTimeBoot: targetDevice=%s, targetEfi=%s", targetDevice, targetEfi))

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
			printDebug(fmt.Sprintf("NVMe partition identified: disk=%s, partition=%s", matches[1], matches[2]))
			// Check if targetDevice matches the disk part
			if matches[1] != targetDevice {
				printDebug(fmt.Sprintf("Warning: Extracted disk %s doesn't match targetDevice %s", matches[1], targetDevice))
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
			printDebug(fmt.Sprintf("Standard partition identified: disk=%s, partition=%s", matches[1], matches[2]))
			// Check if targetDevice matches the disk part
			if matches[1] != targetDevice {
				printDebug(fmt.Sprintf("Warning: Extracted disk %s doesn't match targetDevice %s", matches[1], targetDevice))
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

	printDebug(fmt.Sprintf("Using disk device: %s, partition: %s", targetDevice, partition))

	// Remove only entries that conflict with our target entry
	for _, match := range matches {
		bootNum := match[1]

		// Get more detailed info about the entry
		bootInfo, err := runCommand("efibootmgr", "-v", "-b", bootNum)
		if err != nil {
			printDebug(fmt.Sprintf("[WARNING] Failed to get info for Boot%s: %v", bootNum, err))
			continue
		}

		// Check if the entry contains the same boot path
		if strings.Contains(bootInfo, targetBootPath) {
			printDebug("[INFO] Removing conflicting OneTimeBoot entry: Boot" + bootNum)
			if err := runCommandNoOutput("efibootmgr", "-B", "-b", bootNum); err != nil {
				printDebug(fmt.Sprintf("[WARNING] Failed to remove Boot%s: %v", bootNum, err))
			}
		} else {
			printDebug("[INFO] Keeping non-conflicting OneTimeBoot entry: Boot" + bootNum)
		}
	}

	printDebug("targetDevice: " + targetDevice)
	printDebug("Partition: " + partition)

	printDebug("[INFO] Creating new OneTimeBoot entry")
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
		printDebug("[ERROR] efibootmgr create output: " + createOut.String())
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

	printDebug("[INFO] New OneTimeBoot entry created: Boot" + bootNum)

	// Set BootNext to the created entry
	if err := runCommandNoOutput("efibootmgr", "-n", bootNum); err != nil {
		out2, err2 := runCommand("efibootmgr", "-v")
		if err2 == nil && strings.Contains(out2, "BootNext: "+bootNum) {
			printDebug("BootNext is already set to Boot" + bootNum)
			return nil
		}
		return fmt.Errorf("failed to set BootNext to %s: %v", bootNum, err)
	}

	out3, err3 := runCommand("efibootmgr", "-v")
	if err3 == nil && strings.Contains(out3, "BootNext: "+bootNum) {
		printDebug("BootNext is set to Boot" + bootNum)
		return nil
	}

	return fmt.Errorf("failed to verify BootNext setting for Boot%s", bootNum)
}

// calculateSessionState определяет общий статус сессии на основе результатов тестов и прошивки
func calculateSessionState(testResults []TestResult, flashResults []FlashResult) string {
	// Проверяем критические тесты
	for _, result := range testResults {
		if result.Required && (result.Status == "FAILED" || result.Status == "TIMEOUT") {
			return "failed"
		}
	}

	// Проверяем результаты прошивки
	for _, flashResult := range flashResults {
		if flashResult.Status == "FAILED" {
			return "failed"
		}
	}

	return "pass"
}

func saveLog(log SessionLog, config LogConfig) error {
	if !config.SaveLocal {
		return nil
	}

	logDir := config.LogDir
	if logDir == "" {
		logDir = "logs"
	}

	// Create log directory
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	// Generate filename with state
	timestamp := log.Timestamp.Format("20060102_150405")
	filename := fmt.Sprintf("%s_%s_%s_%s.yaml", log.System.Product, log.System.MBSerial, timestamp, log.State)
	filepath := filepath.Join(logDir, filename)

	// Marshal to YAML
	data, err := yaml.Marshal(log)
	if err != nil {
		return fmt.Errorf("failed to marshal log: %v", err)
	}

	// Write to file
	err = os.WriteFile(filepath, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write log file: %v", err)
	}

	printSuccess(fmt.Sprintf("Log saved: %s", filepath))
	return nil
}

func main() {
	var configPath string
	var showVersion bool
	var testsOnly bool
	var flashOnly bool
	var show_Help bool

	flag.StringVar(&configPath, "c", "config.yaml", "Path to configuration file")
	flag.BoolVar(&showVersion, "V", false, "Show version")
	flag.BoolVar(&testsOnly, "tests-only", false, "Run only tests (skip flashing)")
	flag.BoolVar(&flashOnly, "flash-only", false, "Run only flashing (skip tests)")
	flag.BoolVar(&show_Help, "h", false, "Show help")
	flag.Parse()

	if show_Help {
		showHelp()
		os.Exit(0)
	}
	if showVersion {
		fmt.Println(VERSION)
		os.Exit(0)
	}

	// Enterprise заголовок
	fmt.Printf("%sFIRESTARTER%s Hardware Validation System %sv%s%s\n",
		ColorBlue, ColorReset, ColorGray, VERSION, ColorReset)
	printThickSeparator()

	// Load configuration
	config, err := loadConfig(configPath)
	if err != nil {
		printError(fmt.Sprintf("Failed to load configuration: %v", err))
		os.Exit(1)
	}
	if config.System.RequireRoot && os.Geteuid() != 0 {
		printError("This program requires root privileges")
		os.Exit(1)
	}

	// System configuration display
	fmt.Printf("\n%sSYSTEM CONFIGURATION%s\n", ColorWhite, ColorReset)
	fmt.Printf("  Target Product    : %s%s%s\n", ColorCyan, config.System.Product, ColorReset)
	fmt.Printf("  Manufacturer      : %s%s%s\n", ColorCyan, config.System.Manufacturer, ColorReset)
	fmt.Printf("  Configuration     : %s%s%s\n", ColorYellow, configPath, ColorReset)
	fmt.Printf("  Root Required     : %s%v%s\n", ColorYellow, config.System.RequireRoot, ColorReset)
	fmt.Printf("  Driver Directory  : %s%s%s\n", ColorBlue, config.System.DriverDir, ColorReset)

	sessionStart := time.Now()

	// System identification
	fmt.Printf("\n%sSYSTEM IDENTIFICATION%s\n", ColorWhite, ColorReset)
	printSeparator()
	systemInfo, err := getSystemInfo()
	if err != nil {
		printError(fmt.Sprintf("Failed to get system information: %v", err))
		os.Exit(1)
	}
	fmt.Printf("  Product Name      : %s%s%s\n", ColorCyan, systemInfo.Product, ColorReset)
	fmt.Printf("  Board Serial      : %s%s%s\n", ColorCyan, systemInfo.MBSerial, ColorReset)
	fmt.Printf("  Network Address   : %s%s%s\n", ColorCyan, systemInfo.IP, ColorReset)
	fmt.Printf("  Detection Time    : %s%s%s\n", ColorGray, systemInfo.Timestamp.Format("2006-01-02 15:04:05"), ColorReset)

	// Product compatibility check
	if config.System.Product != "" && systemInfo.Product != "" {
		if config.System.Product != systemInfo.Product {
			if askUserProductMismatch(config.System.Product, systemInfo.Product) {
				printInfo("Program terminated by user due to product mismatch")
				os.Exit(0)
			}
			fmt.Printf("  Configuration     : %sWARNING - Product mismatch%s\n", ColorYellow, ColorReset)
		} else {
			fmt.Printf("  Configuration     : %sCompatible%s\n", ColorGreen, ColorReset)
		}
	} else {
		if config.System.Product == "" {
			fmt.Printf("  Configuration     : %sNo product specified in config%s\n", ColorYellow, ColorReset)
		}
		if systemInfo.Product == "" {
			fmt.Printf("  Configuration     : %sCould not detect system product%s\n", ColorYellow, ColorReset)
		}
	}

	// Test server connection
	if config.Log.SendLogs {
		if err := testServerConnection(config.Log); err != nil {
			printError(fmt.Sprintf("Server connection test failed: %v", err))
			printError("Log sending will be disabled for this session")
			config.Log.SendLogs = false
		}
	}

	var allResults []TestResult
	var flashResults []FlashResult
	var flashData *FlashData

	// TESTING PHASE [1/2]
	if !flashOnly {
		fmt.Printf("\n%sTESTING PHASE [1/2]%s\n", ColorWhite, ColorReset)
		printThickSeparator()

		// Count tests
		totalTests := 0
		for _, g := range config.Tests.ParallelGroups {
			totalTests += len(g)
		}
		for _, g := range config.Tests.SequentialGroups {
			totalTests += len(g)
		}
		fmt.Printf("Total Tests: %s%d%s | Global Timeout: %s%s%s\n",
			ColorGreen, totalTests, ColorReset,
			ColorYellow, func() string {
				if config.Tests.Timeout != "" {
					return config.Tests.Timeout
				}
				return "30s (default)"
			}(), ColorReset)

		// Run tests
		testsStart := time.Now()
		for i, g := range config.Tests.ParallelGroups {
			groupName := fmt.Sprintf("Parallel Group %d", i+1)
			results := runTestGroup(g, true, outputManager, groupName, config.Tests.Timeout)
			allResults = append(allResults, results...)
		}
		for i, g := range config.Tests.SequentialGroups {
			groupName := fmt.Sprintf("Sequential Group %d", i+1)
			results := runTestGroup(g, false, outputManager, groupName, config.Tests.Timeout)
			allResults = append(allResults, results...)
		}
		testsDuration := time.Since(testsStart)

		// Tests summary
		printTestsSummary(allResults, testsDuration)

		// List failed tests by name
		var failedNames []string
		for _, r := range allResults {
			if r.Status == "FAILED" || r.Status == "TIMEOUT" {
				failedNames = append(failedNames, r.Name)
			}
		}
		if len(failedNames) > 0 {
			fmt.Printf("%sFailed tests:%s %s\n\n",
				ColorRed, ColorReset, strings.Join(failedNames, ", "))
		}
	}

	// FLASH data input
	if !testsOnly && config.Flash.Enabled {
		flashData, err = getFlashData(config.Flash, systemInfo.Product)
		if err != nil {
			printError(fmt.Sprintf("Failed to get flash data: %v", err))
			os.Exit(1)
		}
	}

	// FLASHING PHASE [2/2]
	var serialNumberChanged bool = false
	if !testsOnly && config.Flash.Enabled && flashData != nil {
		fmt.Printf("\n%sFLASHING PHASE [2/2]%s\n", ColorWhite, ColorReset)
		printThickSeparator()
		fmt.Printf("Operations: %s%s%s | Method: %s%s%s\n",
			ColorYellow, strings.Join(config.Flash.Operations, ", "), ColorReset,
			ColorGreen, config.Flash.Method, ColorReset)
		flashResults, serialNumberChanged = runFlashing(config.Flash, flashData, config.System)
	}

	// Session duration
	totalDuration := time.Since(sessionStart)

	// Вычисляем общий статус сессии
	sessionState := calculateSessionState(allResults, flashResults)

	// Save & send logs
	sessionLog := SessionLog{
		SessionID:    fmt.Sprintf("%d", time.Now().Unix()),
		Timestamp:    sessionStart,
		State:        sessionState,
		Pipeline:     PipelineInfo{Mode: "full", Config: configPath, Duration: totalDuration, Operator: config.Log.OpName},
		TestResults:  allResults, // Перенесено выше системной информации
		FlashResults: flashResults,
		System:       systemInfo, // Остается внизу, но выше dmidecode
	}

	if flashData != nil {
		// Прошитые значения записываем в основные поля
		if flashData.SystemSerial != "" {
			sessionLog.System.MBSerial = flashData.SystemSerial
		}
		if flashData.IOBoard != "" {
			sessionLog.System.IOSerial = flashData.IOBoard
		}
		if flashData.MAC != "" {
			sessionLog.System.MAC = flashData.MAC
		}

		printInfo("Log will include both original and flashed values")
		printInfo(fmt.Sprintf("  Original MB Serial: %s -> Flashed: %s",
			sessionLog.System.OriginalMBSerial, sessionLog.System.MBSerial))
		if len(sessionLog.System.OriginalMACs) > 0 {
			printInfo(fmt.Sprintf("  Original MACs: %s -> Flashed: %s",
				strings.Join(sessionLog.System.OriginalMACs, ", "), sessionLog.System.MAC))
		}
	} else {
		printInfo("No flashing performed - only original values will be logged")
	}

	if err := saveLog(sessionLog, config.Log); err != nil {
		printError(fmt.Sprintf("Failed to save log: %v", err))
	}
	if config.Log.SendLogs {
		if err := sendLogToServer(sessionLog, config.Log); err != nil {
			printError(fmt.Sprintf("Failed to send log to server: %v", err))
		}
	} else {
		printInfo("Log sending disabled (send_logs: false)")
	}

	// Final summary
	printExecutionSummary(allResults, flashResults, totalDuration)

	// Exit code
	exitCode := 0
	for _, r := range allResults {
		if r.Status == "FAILED" && r.Required {
			exitCode = 1
			break
		}
	}
	for _, fr := range flashResults {
		if fr.Status == "FAILED" {
			exitCode = 1
			break
		}
	}
	if exitCode != 0 {
		fmt.Printf("\n%sExiting with error code %d due to failed critical operations%s\n",
			ColorRed, exitCode, ColorReset)
	}

	reader := bufio.NewReader(os.Stdin)

	if serialNumberChanged {
		// Серийный номер был изменен - требуется перезагрузка
		fmt.Printf("\n%sSerial number was updated. System reboot is required for changes to take effect.%s\n", ColorYellow, ColorReset)
		fmt.Printf("%sDo you want to reboot the system now?%s %s[Y/n]%s: ", ColorWhite, ColorReset, ColorGreen, ColorReset)

		input, err := reader.ReadString('\n')
		if err != nil {
			input = "Y"
		}
		input = strings.TrimSpace(strings.ToUpper(input))

		if input == "" || input == "Y" || input == "YES" {
			printInfo("Preparing system for reboot...")

			if err := bootctl(); err != nil {
				printError("Bootctl error: " + err.Error())
				os.Exit(1)
			}

			printSuccess("System will reboot now...")
			if err := exec.Command("reboot").Run(); err != nil {
				printError(fmt.Sprintf("Failed to reboot: %v", err))
				os.Exit(1)
			}
		} else {
			printInfo("Reboot cancelled by user.")
			printWarning("Note: Serial number changes require a reboot to take effect.")
		}
	} else {
		// Серийный номер не изменялся - можно просто выключить
		fmt.Printf("\n%sNo serial number changes were made. System can be safely shut down.%s\n", ColorBlue, ColorReset)
		fmt.Printf("%sDo you want to shutdown the system now?%s %s[Y/n]%s: ", ColorWhite, ColorReset, ColorGreen, ColorReset)

		input, err := reader.ReadString('\n')
		if err != nil {
			input = "Y"
		}
		input = strings.TrimSpace(strings.ToUpper(input))

		if input == "" || input == "Y" || input == "YES" {
			printInfo("Preparing system for shutdown...")
			printSuccess("System will shutdown now...")
			if err := exec.Command("shutdown", "-h", "now").Run(); err != nil {
				printError(fmt.Sprintf("Failed to shutdown: %v", err))
				os.Exit(1)
			}
		} else {
			printInfo("Shutdown cancelled by user.")
		}
	}

	os.Exit(exitCode)
}
