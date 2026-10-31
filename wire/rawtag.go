package wire

import "strings"

// RawTag renders a raw backend id as the tag half of a namespaced card id:
// last path segment, ".gguf" stripped, charset-reduced to what a model_id
// may carry, bounded to 80 chars. Shared by both sides of the wire so a
// consumer can address one machine's copy of a canonical model as
// "<node_id>/<raw_tag>" and the node, the router, and any UI derive the
// same tag from the same raw id — the node names its unmapped cards with
// it, the router resolves pins with it (nodelink's pin lookup).
func RawTag(raw string) string {
	tag := raw
	if i := strings.LastIndexByte(tag, '/'); i >= 0 {
		tag = tag[i+1:]
	}
	tag = strings.TrimSuffix(tag, ".gguf")
	var b strings.Builder
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			strings.ContainsRune("._:@-", r):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	tag = strings.Trim(b.String(), "-")
	if len(tag) > 80 {
		tag = tag[:80]
	}
	if tag == "" {
		tag = "model"
	}
	return tag
}
