package testsupport

import "testing"

// SingletonValue asserts flag appears exactly once in args and returns its
// value.
func SingletonValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	value, count := "", 0
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			count++
			if i+1 < len(args) {
				value = args[i+1]
			}
		}
	}
	if count != 1 {
		t.Fatalf("argv %#v carries %d %s flags, want exactly 1", args, count, flag)
	}
	return value
}

func ContainsArgPair(argv []string, key, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == key && argv[i+1] == value {
			return true
		}
	}
	return false
}

// ArgumentValue reads a named value from fixture argv.
func ArgumentValue(args []string, flag string) string {
	value := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			value = args[i+1]
			i++
		}
	}
	return value
}
