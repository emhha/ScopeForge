import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router";
import { SessionsTab } from "@/app/(main)/function/tasks/detail/_tabs/sessions-tab";
import { api } from "./api";
import type { Activity } from "./types";
import "@/app/globals.css";

const now = new Date().toISOString();
const history: Record<string, Activity[]> = {
  "main:0": [{seq:1,worker:"mainagent",main_seg:0,kind:"user",summary:"prompt-seed",ts:now}],
  plan: [{seq:2,worker:"planner",kind:"round",summary:"planner-seed",ts:now}],
  "intent:3": [],
};
const received: unknown[] = [];
const loads: string[] = [];
Object.assign(window, { fixture: {history,received,loads} });
const NativeEventSource = window.EventSource;
window.EventSource = class extends NativeEventSource {
  constructor(url: string | URL, options?: EventSourceInit) {
    super(url, options);
    this.addEventListener("message", e => received.push(JSON.parse(e.data)));
  }
};
Object.assign(api, {
  mainSessions: async () => ({sessions:[{seq:0,created_at:now}],current:0}),
  activityHistory: async (_task: string, key: string) => {loads.push(key);return {items:[...(history[key] ?? [])],snapshotCursor:3,hasMore:false,earliestCursor:1}},
  activityDetail: async (seq: number) => ({detail:Object.values(history).flat().find(a=>a.seq===seq)?.detail ?? ""}),
  tokenStats: async () => ({total:{input_tokens:0,output_tokens:0,cache_read_tokens:0,cache_write_tokens:0},sessions:[]}),
  chatStatus: async () => ({running:true}),
  taskLLMResolution: async () => ({}),
  interceptTask: async () => [],
  taskIntentAssets: async () => [],
  intentsPage: async () => ({items:[{id:"3",type:"intent",state:"running",payload:JSON.stringify({summary:"worker-seed"}),ts:now}],hasMore:false}),
});
createRoot(document.getElementById("root")!).render(<MemoryRouter initialEntries={["/function/tasks/detail?id=1"]}><SessionsTab taskId="1" /></MemoryRouter>);
