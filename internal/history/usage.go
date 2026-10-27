package history

import "encoding/json"

func sumInt64Pointers(values ...*int64) *int64 {
	var total int64
	any := false
	for _, value := range values {
		if value == nil {
			continue
		}
		total += *value
		any = true
	}
	if !any {
		return nil
	}
	return int64Pointer(total)
}

func subInt64Pointers(later, earlier *int64) *int64 {
	if later == nil {
		return nil
	}
	if earlier == nil {
		return int64Pointer(*later)
	}
	return int64Pointer(*later - *earlier)
}

func usageHasNegative(usage *TranscriptTokenUsage) bool {
	if usage == nil {
		return false
	}
	for _, value := range []*int64{usage.Input, usage.Output, usage.Reasoning, usage.CacheRead, usage.CacheWrite, usage.Total} {
		if value != nil && *value < 0 {
			return true
		}
	}
	return false
}

func sumUsage(values ...*TranscriptTokenUsage) *TranscriptTokenUsage {
	var result TranscriptTokenUsage
	any := false
	for _, value := range values {
		if value == nil {
			continue
		}
		any = true
		result.Input = sumInt64Pointers(result.Input, value.Input)
		result.Output = sumInt64Pointers(result.Output, value.Output)
		result.Reasoning = sumInt64Pointers(result.Reasoning, value.Reasoning)
		result.CacheRead = sumInt64Pointers(result.CacheRead, value.CacheRead)
		result.CacheWrite = sumInt64Pointers(result.CacheWrite, value.CacheWrite)
		result.Cached = sumInt64Pointers(result.Cached, value.Cached)
		result.Total = sumInt64Pointers(result.Total, value.Total)
		if len(value.Native) > 0 {
			result.Native = append(json.RawMessage(nil), value.Native...)
		}
		if value.ContextWindow != nil {
			result.ContextWindow = int64Pointer(*value.ContextWindow)
		}
	}
	if !any {
		return nil
	}
	result.Contract = UsageContractPerTurnV1
	syncLegacyCached(&result)
	return &result
}

func subUsage(later, earlier *TranscriptTokenUsage) *TranscriptTokenUsage {
	if later == nil {
		return nil
	}
	result := &TranscriptTokenUsage{
		Input:      subInt64Pointers(later.Input, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.Input })),
		Output:     subInt64Pointers(later.Output, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.Output })),
		Reasoning:  subInt64Pointers(later.Reasoning, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.Reasoning })),
		CacheRead:  subInt64Pointers(later.CacheRead, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.CacheRead })),
		CacheWrite: subInt64Pointers(later.CacheWrite, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.CacheWrite })),
		Total:      subInt64Pointers(later.Total, pointerFromUsage(earlier, func(u *TranscriptTokenUsage) *int64 { return u.Total })),
		Contract:   UsageContractPerTurnV1,
	}
	if later.ContextWindow != nil {
		result.ContextWindow = int64Pointer(*later.ContextWindow)
	}
	syncLegacyCached(result)
	return result
}

func pointerFromUsage(usage *TranscriptTokenUsage, field func(*TranscriptTokenUsage) *int64) *int64 {
	if usage == nil {
		return nil
	}
	return field(usage)
}

func syncLegacyCached(usage *TranscriptTokenUsage) {
	if usage == nil {
		return
	}
	if usage.CacheRead != nil {
		usage.Cached = int64Pointer(*usage.CacheRead)
	}
}

func cloneUsage(usage *TranscriptTokenUsage) *TranscriptTokenUsage {
	if usage == nil {
		return nil
	}
	copy := *usage
	copy.Input = cloneInt64Pointer(usage.Input)
	copy.Output = cloneInt64Pointer(usage.Output)
	copy.Reasoning = cloneInt64Pointer(usage.Reasoning)
	copy.CacheRead = cloneInt64Pointer(usage.CacheRead)
	copy.CacheWrite = cloneInt64Pointer(usage.CacheWrite)
	copy.Cached = cloneInt64Pointer(usage.Cached)
	copy.Total = cloneInt64Pointer(usage.Total)
	copy.ContextWindow = cloneInt64Pointer(usage.ContextWindow)
	if len(usage.Native) > 0 {
		copy.Native = append(json.RawMessage(nil), usage.Native...)
	}
	return &copy
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return int64Pointer(*value)
}

func marshalUsageNative(fields map[string]any) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return raw
}
