package transport

import "strings"

// Quote returns the argument as one POSIX shell word. A POSIX shell reads
// every character inside single quotes literally. A single quote inside the
// argument ends the quoted string, adds an escaped quote, and starts a new
// quoted string.
func Quote(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
}

// QuoteCommand joins argv into one command line in which a POSIX shell reads
// each element as one word.
func QuoteCommand(argv []string) string {
	quotedArguments := make([]string, 0, len(argv))
	for _, argument := range argv {
		quotedArguments = append(quotedArguments, Quote(argument))
	}
	return strings.Join(quotedArguments, " ")
}
