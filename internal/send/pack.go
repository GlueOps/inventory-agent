package send

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"

	"github.com/glueops/inventory-agent/internal/schema"
)

// Pack marshals and gzips the envelope. If the gzipped body exceeds
// maxGzipBytes, pod_images rows are dropped from the end until it fits and
// the section is marked truncated. The returned dropped count is the number
// of rows removed. If the body is still too large with zero rows, it is
// returned as-is (the caller logs a warning); nothing else is ever cut.
func Pack(env *schema.Envelope, maxGzipBytes int) (body []byte, dropped int, err error) {
	for {
		body, err = encode(env)
		if err != nil {
			return nil, dropped, err
		}
		if maxGzipBytes <= 0 || len(body) <= maxGzipBytes {
			return body, dropped, nil
		}
		rows := env.Datasets.PodImages.Data
		if len(rows) == 0 {
			return body, dropped, nil
		}
		// Shrink proportionally with a 10% margin, always by at least one row.
		keep := int(float64(len(rows)) * float64(maxGzipBytes) / float64(len(body)) * 0.9)
		if keep >= len(rows) {
			keep = len(rows) - 1
		}
		if keep < 0 {
			keep = 0
		}
		dropped += len(rows) - keep
		env.Datasets.PodImages.Data = rows[:keep:keep]
		env.Datasets.PodImages.Truncated = true
	}
}

func encode(env *schema.Envelope) ([]byte, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip envelope: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip envelope: %w", err)
	}
	return buf.Bytes(), nil
}
