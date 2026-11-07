package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"math/big"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// generateSecret returns a new random value from crypto/rand. Agents never generate
// secrets by writing text (D-030); they ask sb-control to.
func generateSecret(g apps.GenSecret) string {
	n := g.Bytes
	if n <= 0 {
		n = 32
	}
	switch g.Format {
	case "base64":
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	case "alnum":
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		out := make([]byte, n)
		for i := range out {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				panic(err)
			}
			out[i] = alphabet[k.Int64()]
		}
		return string(out)
	default:
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return hex.EncodeToString(b)
	}
}

func secretField(sec kube.Obj, key string) string {
	b, _ := base64.StdEncoding.DecodeString(kube.Str(sec, "data", key))
	return string(b)
}

// dbValues reads the database connection values for config files.
func (s *Server) dbValues(ctx context.Context, a *apps.App) map[string]string {
	if !a.Database {
		return nil
	}
	var sec kube.Obj
	if s.kube.Get(ctx, "v1", "Secret", apps.Namespace(a.Name), apps.DBSecret, &sec) != nil {
		return nil
	}
	return map[string]string{"url": secretField(sec, "uri"), "password": secretField(sec, "password")}
}
