package executor

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live diagnostic for the document-upload leg of context
// externalization: STS (v2 then v1) → signed OSS PUT → parse + poll,
// against the real account named by the environment. It logs statuses
// and field presence only — never a credential value. Guarded by
// QWEN_WEB_LIVE_UPLOAD=1 with QWEN_WEB_TOKEN / QWEN_WEB_COOKIE set.
func TestQwenWebLiveUploadProbe(t *testing.T) {
	if os.Getenv("QWEN_WEB_LIVE_UPLOAD") != "1" {
		t.Skip("live upload probe disabled")
	}
	cred := qwenWebCredential{
		Token:    os.Getenv("QWEN_WEB_TOKEN"),
		Cookie:   os.Getenv("QWEN_WEB_COOKIE"),
		LoggedIn: os.Getenv("QWEN_WEB_TOKEN") != "",
	}
	if !cred.LoggedIn {
		t.Fatal("QWEN_WEB_TOKEN is not set")
	}
	client := qwenWebHTTPClient()
	ctx := context.Background()
	if exp, ok := qwenWebJWTExpiry(cred.Token); ok && time.Now().After(exp) {
		fresh, merged, err := qwenWebMintToken(ctx, client, cred)
		if err != nil {
			t.Fatalf("mint before probe: %v", err)
		}
		cred.Token = fresh
		if merged != "" {
			cred.Cookie = merged
		}
		t.Log("stored token was expired; minted a fresh one for the probe")
	}

	content := "qw-web upload probe 2026-11-03\nsecond line of the probe document\n"
	var sts *qwenWebSTS
	for _, path := range []string{qwenWebSTSPathV2, qwenWebSTSPathV1} {
		got, err := qwenWebGetSTSAt(ctx, client, cred, path, "qw-probe.txt", len(content))
		if err != nil {
			t.Logf("STS %s failed: %v", path, err)
			continue
		}
		sts = got
		t.Logf("STS %s ok: file_id len=%d bucket=%q region=%q endpoint=%q path_len=%d url_set=%v",
			path, len(sts.FileID), sts.Bucket, sts.Region, sts.Endpoint, len(sts.FilePath), sts.FileURL != "")
		break
	}
	if sts == nil {
		t.Fatal("both STS paths failed")
	}
	if err := qwenWebPutObject(ctx, client, sts, []byte(content)); err != nil {
		t.Fatalf("OSS PUT: %v", err)
	}
	t.Log("OSS PUT ok")
	if err := qwenWebParseFile(ctx, client, cred, sts.FileID); err != nil {
		t.Fatalf("parse: %v", err)
	}
	t.Log("parse ok: document is readable upstream")
}
