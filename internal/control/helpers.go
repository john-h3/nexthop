package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func marshalYAML(v any) ([]byte, error) {
	return yaml.Marshal(v)
}

func bytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

func jsonDecode(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}

func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
