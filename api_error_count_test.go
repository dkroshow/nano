package main

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/httpapi"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const plainTextHTTPErrorCeiling = 0

func TestPlainTextHTTPErrorCeiling(t *testing.T) {
	t.Helper()
	errorCount := 0
	notFoundCount := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok {
				packageName, packageOK := selector.X.(*ast.Ident)
				if packageOK && packageName.Name == "http" {
					switch selector.Sel.Name {
					case "Error":
						errorCount++
					case "NotFound":
						notFoundCount++
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if errorCount > plainTextHTTPErrorCeiling || notFoundCount != 0 {
		t.Fatalf("production plain-text helpers: http.Error=%d (ceiling %d), http.NotFound=%d", errorCount, plainTextHTTPErrorCeiling, notFoundCount)
	}
}

func TestWriteHTTPErrorUsesOneJSONEnvelope(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpapi.WriteHTTPError(recorder, "bad input", http.StatusBadRequest)
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	errorValue, ok := body["error"].(map[string]any)
	if !ok || len(body) != 1 || len(errorValue) != 2 || errorValue["code"] != "invalid_request" || errorValue["message"] != "bad input" {
		t.Fatalf("body = %#v", body)
	}
}
