package sessions

// Shared argv-singleton mechanics (coordination decision 12). Claude --resume
// succession, the identity flag's anti-stacking rule, and the posture log's
// resume overlay (posture item 4) all need the same guarantee: at most one
// `flag value` pair survives in the argv.

// argvStripPair removes every `flag <value>` pair — including a dangling
// trailing flag with no value — returning the stripped argv, the last value
// seen, and whether the flag occurred at all.
func argvStripPair(args []string, flag string) (out []string, lastValue string, found bool) {
	out = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			found = true
			if i+1 < len(args) {
				lastValue = args[i+1]
				i++ // drop the flag's value with it
			}
			continue
		}
		out = append(out, args[i])
	}
	return out, lastValue, found
}

// argvSetSingletonPair strips every existing pair for flag, then appends
// exactly one `flag value` at the end.
func argvSetSingletonPair(args []string, flag, value string) []string {
	out, _, _ := argvStripPair(args, flag)
	return append(out, flag, value)
}

// argvCollapseSingletonPair strips repeated pairs for flag and re-appends the
// last stored value once. Argv without the flag — or with only a dangling,
// valueless occurrence — comes back without it.
func argvCollapseSingletonPair(args []string, flag string) []string {
	out, last, found := argvStripPair(args, flag)
	if !found || last == "" {
		return out
	}
	return append(out, flag, last)
}
