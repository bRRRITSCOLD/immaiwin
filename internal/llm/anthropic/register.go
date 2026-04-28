package anthropic

import "github.com/bRRRITSCOLD/immaiwin-go/internal/llm"

// ConnectionType is the connection-type identifier used to register this
// provider in the LLM registry. Workflow connections of this type resolve
// to an Anthropic Provider via the registry's Build function.
const ConnectionType = "anthropic"

// init registers the Anthropic factory at package import time. To use this
// provider, blank-import the package once at process startup:
//
//	import _ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/anthropic"
func init() {
	_ = llm.Register(ConnectionType, New)
}
