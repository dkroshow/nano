package model

import "encoding/json"

// ExactNativeItemID returns the strong identity, or empty for a provisional
// composite. A composite carries its basis in the alias whose scalar value
// equals NativeItemID, in the provider.messageBlock namespace. Native IDs
// without a composite alias remain exact.
// BasisDerived means a counted or otherwise provisional component; only native
// and derived-exact aliases can promote that same composite to a strong key.
func (item Item) ExactNativeItemID() string {
	provisional := false
	for _, alias := range item.Aliases {
		if alias.Namespace != string(item.Scope.Provider)+".messageBlock" {
			continue
		}
		var value string
		if json.Unmarshal(alias.Value, &value) != nil || value != item.NativeItemID {
			continue
		}
		switch alias.Authority {
		case BasisNative, BasisDerivedExact:
			return item.NativeItemID
		case BasisDerived:
			provisional = true
		}
	}
	if provisional {
		return ""
	}
	return item.NativeItemID
}

// ExactNativeItemComponents retains exact parts of a composite even when a
// different component is provisional. A native API message ID can therefore
// distinguish two blocks whose indices were counted. Unrelated envelope aliases
// (prompt IDs, request IDs, UUIDs) are not item-identity components.
func (item Item) ExactNativeItemComponents() []Alias {
	var components []Alias
	prefix := string(item.Scope.Provider) + "."
	for _, alias := range item.Aliases {
		if alias.Authority != BasisNative && alias.Authority != BasisDerivedExact {
			continue
		}
		if alias.Namespace == prefix+"apiMessageId" || alias.Namespace == prefix+"blockIndex" {
			components = append(components, alias)
		}
	}
	return components
}
