package news

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// transpileTS converts TypeScript (or plain JavaScript) source to ES2015-compatible
// JavaScript suitable for execution in goja. Returns the transpiled JS string.
// If the input is plain JS with no TypeScript syntax, esbuild is effectively a no-op.
func transpileTS(script string) (string, error) {
	result := api.Transform(script, api.TransformOptions{
		Loader: api.LoaderTS, // handles TS and JS (TS is a superset of JS)
		Target: api.ES2015,   // goja supports ES2015+
	})

	if len(result.Errors) > 0 {
		msgs := make([]string, 0, len(result.Errors))
		for _, e := range result.Errors {
			loc := ""
			if e.Location != nil {
				loc = fmt.Sprintf("line %d: ", e.Location.Line)
			}
			msgs = append(msgs, loc+e.Text)
		}
		return "", fmt.Errorf("typescript: %s", strings.Join(msgs, "; "))
	}

	return string(result.Code), nil
}
