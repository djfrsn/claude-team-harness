package acp

import (
	"os"
	"strings"
)

var inheritedEnvironment = [...]string{
	"HOME",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LOGNAME",
	"PATH",
	"SHELL",
	"TMPDIR",
	"USER",
}

func adapterEnvironment(dir string, configured []string) []string {
	environment := make([]string, 0, len(inheritedEnvironment)+len(configured)+1)
	positions := make(map[string]int, cap(environment))
	set := func(entry string) {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" {
			environment = append(environment, entry)
			return
		}
		if position, exists := positions[name]; exists {
			environment[position] = entry
			return
		}
		positions[name] = len(environment)
		environment = append(environment, entry)
	}
	for _, name := range inheritedEnvironment {
		if value, exists := os.LookupEnv(name); exists {
			set(name + "=" + value)
		}
	}
	for _, entry := range configured {
		set(entry)
	}
	set("PWD=" + dir)
	return environment
}
