import React from "react";
import { createRoot } from "react-dom/client";
import FlowsPage from "../app/(main)/function/flows/page";
import { flowAPI, type FlowDefinition } from "./flow-api";
import { parallelDomainExample } from "./flow-state";
import "../app/globals.css";

const definition = parallelDomainExample();
definition.nodes.find((node) => node.id === "fork")!.branches = [];
const tools = [
  {
    name: "workflow_command",
    description: "直接执行程序，无须注册",
    input_schema: { type: "object", required: ["program"], properties: { program: { type: "string" } } },
  },
  {
    name: "Bash",
    description: "宿主 shell",
    input_schema: { type: "object", required: ["command"], properties: { command: { type: "string" } } },
  },
];
const fixture = { checked: null as FlowDefinition | null };
Object.assign(window, { fixture });
window.confirm = () => true;
flowAPI.catalog = async () => ({ tools, example: definition, operations: ["lines"] });
flowAPI.list = async () => [];
flowAPI.allRuns = async () => [];
flowAPI.validate = async (def) => {
  fixture.checked = def;
  return { valid: true, diagnostics: [] };
};
createRoot(document.getElementById("root")!).render(<FlowsPage />);
