package deploy

import "github.com/GizClaw/flowcraft/core/utils"

// Parse decodes one deployment document from YAML or JSON into a
// [Document]. It applies the shared strict semantics of core/utils:
// JSON is detected by the Kubernetes rule (first non-whitespace byte
// is an open brace), YAML is converted strictly, unknown fields are
// rejected, and a trailing document is an error. After decoding,
// Parse validates the document semantically (version, resources,
// agents).
func Parse(data []byte) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, err
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}
