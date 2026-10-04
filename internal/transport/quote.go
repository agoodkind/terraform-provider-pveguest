package transport

import "strings"

// Quote returns the argument as one POSIX shell word. A single quote inside
// the argument ends the quoted string, adds an escaped quote, and starts a
// new quoted string, because no escape exists inside single quotes.
func Quote(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
}

// QuoteCommand returns the argument vector as one POSIX shell command line.
func QuoteCommand(argv []string) string {
	quotedArguments := make([]string, 0, len(argv))
	for _, argument := range argv {
		quotedArguments = append(quotedArguments, Quote(argument))
	}
	return strings.Join(quotedArguments, " ")
}
