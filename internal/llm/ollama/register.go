package ollama

import "github.com/bRRRITSCOLD/immaiwin-go/internal/llm"

// ConnectionType is the connection-type identifier used to register this
// provider in the LLM registry. Workflow connections of this type resolve
// to an Ollama Provider via the registry's Build function.
const ConnectionType = "ollama"

// init registers the Ollama factory at package import time. To use this
// provider, blank-import the package once at process startup:
//
//	import _ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/ollama"
func init() {
	_ = llm.Register(ConnectionType, New)
}
