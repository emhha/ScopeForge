import { http } from "./api";

export interface EmbeddingConfig {
  mode?: "none" | "local" | "remote";
  base_url: string;
  model: string;
  version?: string;
  dimensions: number;
  api_key?: string;
  configured: boolean;

}
export interface ComponentStatus {
 model_loaded:boolean; mode:"none"|"local"|"remote"; available:boolean; installed:boolean; model_installed:boolean; installing:boolean;
 stage:string; downloaded:number; total:number; error?:string; platform:string; version?:string; probe_ms?:number;
}
export interface RetrievalStatus {
  mode: "keyword" | "hybrid";
  ready: boolean;
  message: string;
}
export interface MemorySettings {
  checkpoint_enabled: boolean;
  task_enabled: boolean;
  global_enabled: boolean;
  auto_distill: boolean;
  auto_extract: boolean;
}
export interface MemoryStatus {
  settings: MemorySettings;
  retrieval: RetrievalStatus;
  jobs: { pending: number; retrying: number };
}
export interface KnowledgeDocument {
  id: string;
  import_id:string;
  relative_path:string;
  name: string;
  size: number;
  status: string;
  error: string;
  chunks: number;
  created_at: string;
}
export interface MemoryEntry {
  id: string;
  memory: { memory: string; topics?: string[] };
  updated_at: string;
}
export interface KnowledgeSearchResult {
  Documents?: { Document: { id: string; content: string; metadata?: Record<string, unknown> }; Score: number }[];
}
export interface KnowledgeImportItem {id:string;relative_path:string;name:string;size:number;status:string;error:string;document_id:string;}
export interface KnowledgeImport {id:string;name:string;kind:"directory"|"zip";status:string;skipped:number;total:number;ready:number;failed:number;pending:number;duplicates:number;cancelled:number;items?:KnowledgeImportItem[];}
export const learningAPI = {
 retrievalStatus:()=>http<RetrievalStatus>("/retrieval/status"),
 imports:()=>http<KnowledgeImport[]>("/knowledge/imports"),
 importItems:(id:string)=>http<{status:string;items:KnowledgeImportItem[]}>(`/knowledge/imports/${encodeURIComponent(id)}`),
 previewDirectory:(selected:File[])=>{const files=selected.filter(f=>/\.(md|txt|docx)$/i.test(f.name));const body=new FormData();body.append("kind","directory");body.append("skipped",String(selected.length-files.length));body.append("name",selected[0]?.webkitRelativePath?.split("/")[0]||"所选文档");body.append("paths",JSON.stringify(files.map(f=>f.webkitRelativePath||f.name)));for(const f of files)body.append("files",f);return http<KnowledgeImport>("/knowledge/imports/preview",{method:"POST",body});},
 previewZIP:(file:File)=>{const body=new FormData();body.append("kind","zip");body.append("name",file.name);body.append("archive",file);return http<KnowledgeImport>("/knowledge/imports/preview",{method:"POST",body});},
 startImport:(id:string)=>http(`/knowledge/imports/${encodeURIComponent(id)}/start`,{method:"POST"}),
 cancelImport:(id:string)=>http(`/knowledge/imports/${encodeURIComponent(id)}/cancel`,{method:"POST"}),
 retryImport:(id:string)=>http(`/knowledge/imports/${encodeURIComponent(id)}/retry`,{method:"POST"}),

 component:()=>http<ComponentStatus>("/retrieval/component"),
 installComponent:(local:boolean)=>http<ComponentStatus>("/retrieval/component/install",{method:"POST",body:JSON.stringify({local})}),
 importComponent:(file:File,local:boolean)=>{const body=new FormData();body.append("file",file);body.append("local",String(local));return http<ComponentStatus>("/retrieval/component/import",{method:"POST",body});},
  status: () => http<RetrievalStatus>("/knowledge/status"),
  testConfig: (value: EmbeddingConfig) =>
    http<EmbeddingConfig>("/retrieval/test", { method: "POST", body: JSON.stringify(value) }),
  memoryStatus: () => http<MemoryStatus>("/memories/status"),
  memoryConfig: (value: MemorySettings) =>
    http<MemorySettings>("/memories/config", { method: "PUT", body: JSON.stringify(value) }),
  config: () => http<EmbeddingConfig>("/retrieval/config"),
  configure: (value: EmbeddingConfig) =>
    http<EmbeddingConfig>("/retrieval/config", { method: "PUT", body: JSON.stringify(value) }),
  documents: () => http<KnowledgeDocument[]>("/knowledge/documents"),
  upload: (file: File) => {
    const body = new FormData();
    body.append("file", file);
    return http<KnowledgeDocument>("/knowledge/documents", { method: "POST", body });
  },
  retry: (id: string) => http(`/knowledge/documents/${encodeURIComponent(id)}/retry`, { method: "POST" }),
  deleteDocument: (id: string) => http(`/knowledge/documents/${encodeURIComponent(id)}`, { method: "DELETE" }),
  search: (query: string) =>
    http<KnowledgeSearchResult>("/knowledge/search", { method: "POST", body: JSON.stringify({ query }) }),
  memories: (taskId?: string) => http<MemoryEntry[]>(taskId ? `/tasks/${taskId}/memories` : "/memories/global"),
  addMemory: (taskId: string, content: string) =>
    http(`/tasks/${taskId}/memories`, { method: "POST", body: JSON.stringify({ content }) }),
  deleteMemory: (id: string, taskId?: string) =>
    http(
      taskId ? `/tasks/${taskId}/memories/${encodeURIComponent(id)}` : `/memories/global/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    ),
  distill: (taskId: string) => http<{ saved: string[] }>(`/tasks/${taskId}/memories/distill`, { method: "POST" }),
};
