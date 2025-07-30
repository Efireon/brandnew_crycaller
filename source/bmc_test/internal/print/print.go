package print

import "fmt"

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[92m"
	colorBlue   = "\033[34m"
	colorWhite  = "\033[37m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

func printColored(color, str string) {
	fmt.Printf("%s%s%s\n", color, str, colorReset)
}

func Debug(str string) {
	printColored(colorWhite, str)
}

func Success(str string) {
	printColored(colorGreen, str)
}

func Fail(str string) {
	printColored(colorRed, str)
}

func Info(str string) {
	printColored(colorBlue, str)
}

func Warning(str string) {
	printColored(colorYellow, str)
}
