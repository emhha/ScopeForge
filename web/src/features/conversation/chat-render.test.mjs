import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { createServer } from "vite";

// Use the application's resolver without opening a listening HTTP server.
test("chat keeps message bubbles and collapsed process groups; expanded steps stay inline", async () => {
  const server = await createServer({ server: { middlewareMode: true, hmr: false }, appType: "custom" });
  try {
    const { Transcript } = await server.ssrLoadModule("/src/components/transcript.tsx");
    const { ConversationStep } = await server.ssrLoadModule("/src/features/conversation/trajectory-view.tsx");
    const { projectTrajectory } = await server.ssrLoadModule("/src/features/conversation/projections.ts");
    const activity = ["user", "thinking", "model_call", "tool_use", "tool_result", "result"].map((kind, index) => ({
      seq: index + 1,
      kind,
      worker: "main",
      ts: "2026-11-06T00:00:00Z",
      summary: kind,
      tool_use_id: kind.startsWith("tool_") ? "call1" : undefined,
      metadata: kind === "model_call" ? { llm_call: { model: "test-model", status: "ok" } } : undefined,
    }));
    const html = renderToStaticMarkup(createElement(Transcript, { activity, scopeKey: "render-regression" }));
    assert.match(html, /standard-chat-view/);
    assert.match(html, /已完成的执行过程/);
    assert.match(html, /1 次工具调用/);
    assert.match(html, /1 段思考/);
    assert.doesNotMatch(html, /<table|border-dashed|查看轨迹|实际模型请求记录/);
    assert.doesNotMatch(html, /输入参数|加载详情/);

    const records = projectTrajectory("render-regression", activity);
    let detailReads = 0;
    const step = renderToStaticMarkup(createElement(ConversationStep, {
      record: records.find((record) => record.event.kind === "model_call"),
      getDetail: async () => { detailReads++; return ""; },
      focused: false,
      initiallyOpen: true,
      onOpenChange: () => {},
    }));
    assert.match(step, /系统提示词/);
    assert.match(step, /加载详情/);
    assert.doesNotMatch(step, /查看轨迹|<table|border-dashed|实际模型请求记录/);
    assert.equal(detailReads, 0, "details are read in the browser effect, not during render");
  } finally {
    await server.close();
  }
});
