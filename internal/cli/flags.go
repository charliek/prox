package cli

import "fmt"

// parseFlagValue returns the value for a flag that requires an argument.
// Returns the value, updated index, and error if no argument follows the flag.
func parseFlagValue(args []string, i int, flagName string) (string, int, error) {
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s requires an argument", flagName)
	}
	return args[i+1], i + 1, nil
}
