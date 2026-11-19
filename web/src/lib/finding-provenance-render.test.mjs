import { createElement } from "react";

import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { createServer } from "vite";

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// Middleware mode compiles components without starting a replacement web server.
test("finding lineage stays compact, retains node drawer links, and reports load errors", async () => {
  const server = await createServer({ server: { middlewareMode: true, hmr: false }, appType: "custom" });
  try {
    const { SourceSessionList, NodeSourceSessions } = await server.ssrLoadModule(
      "/src/app/(main)/function/findings/detail/provenance.tsx",
    );
    const { FindingLineageView } = await server.ssrLoadModule("/src/app/(main)/function/findings/detail/lineage.tsx");
    const { collapseDigestGraph } = await server.ssrLoadModule("/src/components/exploration-graph.tsx");
    const sessions = [
      {
        task_id: "9",
        session: "intent:10",
        title: "接口探索",
        role: "worker",
        contributions: [{ node_id: "11", relation: "produced", precision: "event", seq: 70, summary: "发现接口" }],
      },
      {
        task_id: "9",
        session: "intent:20",
        title: "权限验证",
        role: "worker",
        contributions: [{ node_id: "21", relation: "produced", precision: "legacy", summary: "确认越权" }],
      },
    ];
    const render = (component, props) =>
      renderToStaticMarkup(createElement(MemoryRouter, null, createElement(component, props)));
    const html = render(FindingLineageView, {
      lineage: { data: { sessions, edges: [], nodes: [{ id: "11", type: "fact", payload: "接口" }] } },
    });
    assert.match(html, /连线标注来源与执行会话/);
    assert.match(html, /点击节点只看该节点的会话/);
    assert.doesNotMatch(html, /发现溯源|个关联会话|打开会话|接口探索|权限验证/);
    const overview = await readFile(
      new URL("../app/(main)/function/findings/detail/page.tsx", import.meta.url),
      "utf8",
    );
    assert.doesNotMatch(overview, /FindingSessions/);
    const drawer = render(SourceSessionList, { sessions, nodeId: "11" });
    assert.match(drawer, /接口探索/);
    assert.match(drawer, /activity=70/);
    assert.doesNotMatch(drawer, /权限验证/);
    const findingDrawer = render(NodeSourceSessions, { sessions, node: { id: "21", type: "finding" } });
    assert.match(findingDrawer, /此节点的来源与执行会话/);
    assert.doesNotMatch(findingDrawer, /接口探索/);
    assert.match(findingDrawer, /权限验证/);
    const factDrawer = render(NodeSourceSessions, { sessions, node: { id: "11", type: "fact" } });
    assert.match(factDrawer, /接口探索/);
    assert.doesNotMatch(factDrawer, /权限验证/);
    const legacy = render(SourceSessionList, { sessions, nodeId: "21" });
    assert.match(legacy, /实际提交会话未记录/);
    assert.doesNotMatch(legacy, /activity=/);
    const unknown = render(SourceSessionList, { sessions, nodeId: "30" });
    assert.match(unknown, /来源会话未记录/);
    assert.doesNotMatch(unknown, /href=/);
    const failure = render(FindingLineageView, {
      lineage: {
        error: "network error",
        retry: () => {
          /* Render-only test. */
        },
      },
    });
    assert.match(failure, /无法加载发现链路/);
    assert.match(failure, /重试/);
    assert.doesNotMatch(failure, /来源会话未记录|react-flow/);
    assert.match(render(FindingLineageView, { lineage: {} }), /正在加载发现链路/);
    assert.match(
      render(FindingLineageView, { lineage: { data: { sessions: [], nodes: [], edges: [] } } }),
      /无链路可展示/,
    );

    // Folding multiple destinations onto one digest must retain every original
    // edge for session labels, without mutating the API data or guessing a digest session.
    const nodes = [
      { id: "1", type: "intent" },
      { id: "2", type: "fact" },
      { id: "3", type: "fact" },
      { id: "10", type: "digest", state: "active" },
    ];
    const edges = [
      { src: "10", dst: "2", rel: "covers" },
      { src: "10", dst: "3", rel: "covers" },
      { src: "1", dst: "2", rel: "yields" },
      { src: "1", dst: "3", rel: "yields" },
    ];
    for (const edge of edges) Object.freeze(edge);
    const folded = collapseDigestGraph(nodes, Object.freeze(edges));
    assert.equal(folded.viewEdges.length, 1);
    assert.equal(folded.viewEdges[0].dst, "10");
    assert.deepEqual(folded.viewEdges[0].originals, [edges[2], edges[3]]);
    assert.equal(edges[2].dst, "2");
    const plain = collapseDigestGraph(nodes.slice(0, 3), edges.slice(2));
    assert.equal(plain.viewEdges.length, 2);
    assert.deepEqual(plain.viewEdges[0].originals, [edges[2]]);
    const expanded = collapseDigestGraph(nodes, edges, false);
    assert.deepEqual(
      expanded.viewNodes.map((n) => n.id),
      ["1", "2", "3"],
    );
    assert.deepEqual(
      expanded.viewEdges.map(({ src, dst }) => [src, dst]),
      [
        ["1", "2"],
        ["1", "3"],
      ],
    );
    // An explicit causal digest must not vanish just because finding traces do
    // not collapse compression membership. Its original dependency is evidence.
    const causal = collapseDigestGraph(nodes, [...edges, { src: "10", dst: "1", rel: "derived_from" }], false);
    assert.equal(
      causal.viewNodes.some((n) => n.id === "10"),
      true,
    );
    assert.equal(
      causal.viewEdges.some((e) => e.src === "10" && e.dst === "1"),
      true,
    );
  } finally {
    await server.close();
  }
});
