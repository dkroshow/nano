// One sidebar popup at a time. It stores identity, not a catalog object, so
// runtime/catalog refreshes are reflected while the popup is open.
export const quickDetail = $state({ target: null })

export function openQuickDetail(target) {
  quickDetail.target = { ...target }
}

export function closeQuickDetail() {
  quickDetail.target = null
}
