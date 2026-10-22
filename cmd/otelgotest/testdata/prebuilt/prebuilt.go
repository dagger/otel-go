package prebuilt

// Echo gives the tests some code to cover.
func Echo(s string) string {
	if s == "" {
		return "empty"
	}
	return s
}
