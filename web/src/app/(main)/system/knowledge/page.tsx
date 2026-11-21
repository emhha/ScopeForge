import { useCallback, useEffect, useRef, useState } from "react";
import { BookOpen, FileText, RefreshCw, Search, Trash2, Upload } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { KnowledgeImports } from "@/components/knowledge-imports";
import {Link} from "react-router";
import { Badge } from "@/components/ui/badge";
import {
  learningAPI,
  type KnowledgeDocument,
  type KnowledgeSearchResult,
  type RetrievalStatus,
} from "@/lib/learning-api";

const statusNames: Record<string, string> = {
  queued: "等待入库",
  indexing: "索引中",
  ready: "可检索",
  failed: "入库失败",
 cancelled:"已取消",
};
export default function KnowledgePage() {
  const [status, setStatus] = useState<RetrievalStatus | null>(null);
  const [documents, setDocuments] = useState<KnowledgeDocument[]>([]);
  const [documentFilter,setDocumentFilter]=useState("");
  const [documentPage,setDocumentPage]=useState(0);
  const filteredDocuments=documents.filter(d=>(d.relative_path||d.name).toLowerCase().includes(documentFilter.toLowerCase()));
  const [busy, setBusy] = useState(false);
  const [searching, setSearching] = useState(false);
  const [query, setQuery] = useState("");
  const [result, setResult] = useState<KnowledgeSearchResult | null>(null);
  const [error, setError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);
  const load = useCallback(async () => {
    try {
      const [docs, retrieval] = await Promise.all([learningAPI.documents(), learningAPI.status()]);
      setDocuments(docs);
      setStatus(retrieval);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载失败");
    }
  }, []);
  useEffect(() => {
    void load();
    const interval = setInterval(() => void load(), 3000);
    return () => {
      clearInterval(interval);
    };
  }, [load]);
  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    try {
      await fn();
      await load();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "操作失败");
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-6 p-4 md:p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <BookOpen className="size-5" />
            知识库
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            上传参考文档，供所有 Agent 按需检索。支持 Markdown、TXT、DOCX，单文件不超过 20 MiB。
          </p>
        </div>
        <Button disabled={busy} onClick={() => fileRef.current?.click()}>
          <Upload className="size-4" />
          上传文档
        </Button>
        <input
          ref={fileRef}
          type="file"
          accept=".md,.txt,.docx"
          className="hidden"
          aria-label="选择入库文档"
          onChange={(e) => {
            const f = e.target.files?.[0];
            if (f)
              void act(async () => {
                await learningAPI.upload(f);
                toast.success("已上传，正在建立关键词索引；语义索引将在配置可用时自动补建");
              });
            e.target.value = "";
          }}
        />
      </div>
      <div role="status" className="rounded-xl border bg-muted/30 p-4 text-sm">
        {status?.message ?? "正在读取检索状态…"}
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border p-4 text-sm"><p>任务记忆、全局经验和知识库共用检索配置。</p><Link className="text-primary underline" to="/system/settings?tab=retrieval">前往模型与检索设置</Link></div>
      <KnowledgeImports />
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      <Card>
        <CardHeader>
          <CardTitle>文档</CardTitle>
          <CardDescription>{documents.length} 个文档 · 仅入库完成的文档参与检索</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {documents.length === 0 && (
            <div className="rounded-lg border border-dashed p-8 text-center text-muted-foreground">
              上传第一份文档，开始构建知识库。
            </div>
          )}
          <Input aria-label="筛选知识库文档" value={documentFilter} onChange={e=>{setDocumentFilter(e.target.value);setDocumentPage(0)}} placeholder="按文件名或相对路径筛选" />
          {filteredDocuments.slice(documentPage*25,(documentPage+1)*25).map((doc) => (
            <div key={doc.id} className="flex flex-wrap items-center gap-3 rounded-lg border p-4">
              <FileText className="size-5 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <p className="break-words font-medium">{doc.relative_path || doc.name}</p>
                <p className="mt-1 text-sm text-muted-foreground">
                  {(doc.size / 1024).toFixed(1)} KiB · {doc.chunks} 个分片
                </p>
                {doc.error && (
                  <p role="alert" className="mt-1 text-sm text-destructive">
                    {doc.error}
                  </p>
                )}
              </div>
              <Badge variant={doc.status === "failed" ? "destructive" : "secondary"}>
                {statusNames[doc.status] ?? doc.status}
              </Badge>
              <Button
                variant="outline"
                size="sm"
                disabled={busy || doc.status === "indexing" || doc.status === "cancelled"}
                onClick={() => void act(() => learningAPI.retry(doc.id))}
              >
                <RefreshCw className="size-4" />
                {doc.status === "failed" ? "重试" : "重建"}
              </Button>
              <Button
                variant="ghost"
                size="icon"
                aria-label={`删除 ${doc.name}`}
                disabled={busy}
                onClick={() =>
                  void act(async () => {
                    await learningAPI.deleteDocument(doc.id);
                    setResult(null);
                  })
                }
              >
                <Trash2 className="size-4" />
              </Button>
            </div>
          ))}
          <div className="flex items-center justify-between gap-2 text-sm"><span>{filteredDocuments.length} 个匹配文档 · 每页 25 个</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={documentPage===0} onClick={()=>setDocumentPage(p=>p-1)}>上一页</Button><Button size="sm" variant="outline" disabled={(documentPage+1)*25>=filteredDocuments.length} onClick={()=>setDocumentPage(p=>p+1)}>下一页</Button></div></div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>检索测试</CardTitle>
          <CardDescription>查看 Agent 检索时能够获得的文档片段和来源。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              setSearching(true);
              setResult(null);
              void learningAPI
                .search(query)
                .then(setResult)
                .catch((e) => toast.error(e instanceof Error ? e.message : "检索失败"))
                .finally(() => setSearching(false));
            }}
          >
            <Input
              aria-label="检索问题"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="输入问题或关键词"
            />
            <Button type="submit" disabled={searching || !query.trim()}>
              <Search className="size-4" />
              {searching ? "检索中…" : "检索"}
            </Button>
          </form>
          {result && !result.Documents?.length && <p className="text-sm text-muted-foreground">没有命中可用文档。</p>}
          {result?.Documents?.map((hit) => (
            <article key={hit.Document.id} className="rounded-lg border p-4">
              <p className="mb-2 text-sm font-medium">
                {String(hit.Document.metadata?.relative_path || hit.Document.metadata?.file_name || "文档")} ·{" "}
                {hit.Document.metadata?.retrieval_mode === "hybrid" ? "语义增强" : "关键词匹配"}
              </p>
              <p className="whitespace-pre-wrap break-words text-sm leading-7">{hit.Document.content}</p>
            </article>
          ))}
        </CardContent>
      </Card>
    </div>
  );
}
