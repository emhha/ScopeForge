package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestNativeCallObserverCapturesActualPromptAndOutcomesWithoutBodyRecording(t *testing.T) {
	for _, outcome := range []string{"ok", "start_error", "response_error", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			var records []CallRecord
			ctx, cancel := context.WithCancel(WithCallObserver(t.Context(), func(record CallRecord) { records = append(records, record) }))
			defer cancel()
			inner := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
				if outcome == "start_error" {
					return nil, errors.New("fixture start failure")
				}
				if outcome == "cancelled" {
					cancel()
					return modeltest.Responses(), nil
				}
				if outcome == "response_error" {
					return modeltest.Responses(&model.Response{Error: &model.ResponseError{Message: "fixture error"}}), nil
				}
				final := modeltest.Final("ok")
				final.Usage = &model.Usage{PromptTokens: 8, CompletionTokens: 3}
				return modeltest.Responses(final), nil
			})
			recorder := Wrap(inner, nil, "actual-model", "actual-profile", "", "", func() bool { return false }).WithProfileID(31)
			req := &model.Request{Messages: []model.Message{{Role: model.RoleSystem, Content: "  actual prompt  "}, {Role: model.RoleUser, Content: "private user content"}, {Role: model.RoleSystem, Content: "second prompt"}}}
			stream, err := recorder.GenerateContent(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			for range stream {
			}
			if len(records) != 1 {
				t.Fatalf("records=%+v", records)
			}
			record := records[0]
			expected := "ok"
			if outcome == "cancelled" {
				expected = "interrupted"
			} else if outcome != "ok" {
				expected = "error"
			}
			if record.Status != expected || record.Model != "actual-model" || record.ProfileID != 31 || record.SystemPrompt != "  actual prompt  \n\nsecond prompt" || record.RequestID == "" {
				t.Fatalf("record=%+v", record)
			}
			data, _ := json.Marshal(record)
			if strings.Contains(string(data), "actual prompt") || strings.Contains(string(data), "private user") {
				t.Fatal("request prompt leaked into lightweight metadata")
			}
			if outcome == "ok" && (record.Input == nil || *record.Input != 8 || record.Output == nil || *record.Output != 3) {
				t.Fatalf("usage=%+v", record)
			}
		})
	}
}
