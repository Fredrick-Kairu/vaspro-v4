// Command checkvectors checks the Go snippet against VASPRO's golden
// vectors (test/contract runs it in the official golang image).
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/docs/webhooks/verify/go/vasprowebhook"
)

type vector struct {
	Name       string `json:"name"`
	Secret     string `json:"secret"`
	Now        int64  `json:"now"`
	Timestamp  string `json:"timestamp"`
	Signature  string `json:"signature"`
	BodyBase64 string `json:"body_base64"`
	Expect     string `json:"expect"`
}

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	var file struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	passed := 0
	for _, v := range file.Vectors {
		body, err := base64.StdEncoding.DecodeString(v.BodyBase64)
		if err != nil {
			fmt.Printf("FAIL %s: bad body_base64: %v\n", v.Name, err)
			continue
		}
		got := outcome(vasprowebhook.Verify(v.Secret, v.Timestamp, v.Signature, body, time.Unix(v.Now, 0)))
		if got != v.Expect {
			fmt.Printf("FAIL %s: got %s, want %s\n", v.Name, got, v.Expect)
			continue
		}
		passed++
	}
	fmt.Printf("go: %d/%d vectors ok\n", passed, len(file.Vectors))
	if passed != len(file.Vectors) {
		os.Exit(1)
	}
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, vasprowebhook.ErrBadTimestamp):
		return "bad_timestamp"
	case errors.Is(err, vasprowebhook.ErrBadSignature):
		return "bad_signature"
	default:
		return "error: " + err.Error()
	}
}
