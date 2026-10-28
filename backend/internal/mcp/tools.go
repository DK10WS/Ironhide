// Package mcp exposes Ironhide's operations as MCP tools.
//
// It is an adapter over package core, the same way package api is, so both
// transports accept exactly what core accepts. Nothing here decides what a
// valid write is.
package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/upload"
)

// Register adds Ironhide's tools to a server.
//
// Only the anonymous operations are exposed. Listing, deleting, pausing and
// redeploying all act on an account, and an MCP client has no session.
func Register(s *sdk.Server, c *core.Core) {
	t := &tools{core: c}

	sdk.AddTool(s, &sdk.Tool{
		Name: "upload_site",
		Description: "Deploy a directory or a .zip file as a site and return its public URL. " +
			"The result is an anonymous drop that stops being served when it expires, unless " +
			"claim_site is called with the returned claim_url. .git and node_modules are skipped " +
			"when a directory is given.",
	}, t.uploadSite)

	sdk.AddTool(s, &sdk.Tool{
		Name: "claim_site",
		Description: "Make an anonymous drop permanent. Takes the claim token or the full " +
			"claim URL returned by upload_site. A token can be redeemed once.",
	}, t.claimSite)
}

type tools struct {
	core *core.Core
}

type uploadSiteInput struct {
	Path string `json:"path" jsonschema:"Directory or .zip file to deploy. Relative paths are resolved against the server's working directory."`
}

type uploadSiteOutput struct {
	URL       string `json:"url" jsonschema:"Public address of the deployed site"`
	ClaimURL  string `json:"claim_url,omitempty" jsonschema:"Single-use link that makes the drop permanent"`
	ExpiresAt string `json:"expires_at,omitempty" jsonschema:"When an unclaimed drop stops being served, RFC 3339"`
	Bytes     int64  `json:"bytes" jsonschema:"Total bytes stored"`
	Files     int    `json:"files" jsonschema:"Number of files stored"`
}

func (t *tools) uploadSite(ctx context.Context, _ *sdk.CallToolRequest, in uploadSiteInput) (*sdk.CallToolResult, uploadSiteOutput, error) {
	payload, err := payloadFor(in.Path, t.core.ArchiveLimit())
	if err != nil {
		return nil, uploadSiteOutput{}, err
	}

	res, err := t.core.Upload(ctx, core.Target{}, payload)
	if err != nil {
		return nil, uploadSiteOutput{}, err
	}

	out := uploadSiteOutput{
		URL:      res.PreviewURL,
		ClaimURL: res.ClaimURL,
		Bytes:    res.Bytes,
		Files:    len(res.Files),
	}
	if res.ExpiresAt != nil {
		out.ExpiresAt = res.ExpiresAt.UTC().Format(time.RFC3339)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "Deployed %d files (%d bytes) to %s", out.Files, out.Bytes, out.URL)
	if out.ClaimURL != "" {
		fmt.Fprintf(&text, "\nClaim it to stop it expiring at %s: %s", out.ExpiresAt, out.ClaimURL)
	}

	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: text.String()}},
	}, out, nil
}

type claimSiteInput struct {
	Token string `json:"token" jsonschema:"The claim token, or the full claim URL it appears in"`
}

type claimSiteOutput struct {
	ID        uint   `json:"id" jsonschema:"Deployment id"`
	URL       string `json:"url" jsonschema:"Public address of the site"`
	ClaimedAt string `json:"claimed_at" jsonschema:"When the token was redeemed, RFC 3339"`
}

func (t *tools) claimSite(ctx context.Context, _ *sdk.CallToolRequest, in claimSiteInput) (*sdk.CallToolResult, claimSiteOutput, error) {
	claimed, err := t.core.Claim(ctx, claimToken(in.Token))
	if err != nil {
		return nil, claimSiteOutput{}, err
	}

	out := claimSiteOutput{
		ID:        claimed.ID,
		URL:       claimed.URL,
		ClaimedAt: claimed.ClaimedAt.UTC().Format(time.RFC3339),
	}

	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{
			Text: fmt.Sprintf("Claimed %s. It will now be served indefinitely.", out.URL),
		}},
	}, out, nil
}

// claimToken accepts either a bare token or the URL it appears in, because a
// caller copying from the upload result will have the URL.
func claimToken(value string) string {
	value = strings.TrimSpace(value)
	const marker = "/claim/"
	if i := strings.LastIndex(value, marker); i >= 0 {
		return strings.TrimSuffix(value[i+len(marker):], "/")
	}
	return value
}

// payloadFor turns a path into something core can deploy.
func payloadFor(path string, limit int64) (core.Payload, error) {
	info, err := os.Stat(path)
	if err != nil {
		return core.Payload{}, fmt.Errorf("read %s: %w", path, err)
	}

	if info.IsDir() {
		data, err := archiveDir(path, limit)
		if err != nil {
			return core.Payload{}, err
		}
		return core.Payload{Zip: data}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return core.Payload{}, fmt.Errorf("read %s: %w", path, err)
	}
	if core.IsArchive(data) {
		// Flattening is what the web uploader always asks for, and it is what
		// makes an archive exported by a hosting service or git deployable: its
		// files sit one directory down, so without it there is no root index.
		return core.Payload{Zip: data, Flatten: true}, nil
	}

	return core.Payload{
		Files: []upload.IncomingFile{{
			Name: filepath.Base(path),
			Data: data,
			Size: int64(len(data)),
		}},
	}, nil
}

// archiveDir builds an archive from a directory without touching the disk.
//
// A directory that is too large is refused while it is being read, so a
// mistaken path cannot buffer more than the upload would accept.
func archiveDir(root string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if p != root && skippedDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate

		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}

		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}

		if limit > 0 && int64(buf.Len()) > limit {
			return fmt.Errorf("archive exceeds %d bytes", limit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// skippedDir names directories that are never part of a deployed site.
func skippedDir(name string) bool {
	return name == ".git" || name == "node_modules"
}
