import { useCallback, useEffect, useRef, useState } from "react";
import { Archive, FolderOpen, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { learningAPI, type KnowledgeImport, type KnowledgeImportItem } from "@/lib/learning-api";

const states: Record<string, string> = { preview: "等待确认", queued: "等待入库", importing: "正在入库", done: "已完成", cancelled: "已取消", staged: "待入库", indexing: "正在解析", ready: "可检索", failed: "失败", duplicate: "已存在，跳过", deleted: "文档已删除" };
export function KnowledgeImports() {
  const folder = useRef<HTMLInputElement>(null);
  const archive = useRef<HTMLInputElement>(null);
  const [batches, setBatches] = useState<KnowledgeImport[]>([]);
  const [preview, setPreview] = useState<KnowledgeImport | null>(null);
  const [details, setDetails] = useState<{ id: string; items: KnowledgeImportItem[] } | null>(null);
  const [filter, setFilter] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const load = useCallback(async () => { try { setBatches(await learningAPI.imports()); setError(""); } catch (e) { setError(e instanceof Error ? e.message : "批次读取失败"); } }, []);
  useEffect(() => { void load(); const timer = setInterval(() => void load(), 2000); return () => clearInterval(timer); }, [load]);
  async function act(action: () => Promise<unknown>) { setBusy(true); try { await action(); await load(); } catch (e) { toast.error(e instanceof Error ? e.message : "操作失败"); } finally { setBusy(false); } }
  async function inspect(batch: KnowledgeImport) { const data = await learningAPI.importItems(batch.id); setFilter(""); if (batch.status === "preview") setPreview({ ...batch, items: data.items }); else setDetails({ id: batch.id, items: data.items }); }
  const visible = details?.items.filter(item => !filter || item.relative_path.toLowerCase().includes(filter.toLowerCase()) || item.error.includes(filter)) ?? [];
  return <Card>
    <CardHeader><CardTitle>批量导入本地资料</CardTitle><CardDescription>选择某个章节的目录或 ZIP，预览后确认入库。保留目录结构，只处理 Markdown、TXT、DOCX。</CardDescription></CardHeader>
    <CardContent className="space-y-4">
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" disabled={busy} onClick={() => folder.current?.click()}><FolderOpen className="size-4" />选择文档目录</Button>
        <Button variant="outline" disabled={busy} onClick={() => archive.current?.click()}><Archive className="size-4" />选择文档 ZIP</Button>
        <input ref={node => { folder.current = node; node?.setAttribute("webkitdirectory", ""); }} type="file" multiple className="hidden" aria-label="选择文档目录" onChange={e => { const files = Array.from(e.target.files ?? []); e.target.value = ""; if (files.length) void act(async () => { const next = await learningAPI.previewDirectory(files); setPreview(next); }); }} />
        <input ref={archive} type="file" accept=".zip" className="hidden" aria-label="选择文档 ZIP" onChange={e => { const file = e.target.files?.[0]; e.target.value = ""; if (file) void act(async () => { const next = await learningAPI.previewZIP(file); setPreview(next); }); }} />
      </div>
      <p className="text-sm text-muted-foreground">每批最多 2000 个文档，单篇不超过 20 MiB，总文档不超过 500 MiB；ZIP 不超过 200 MiB。图片等文件会跳过。没有模型也能完成入库。</p>
      {busy && <p role="status" className="text-sm">正在上传、扫描或处理，请稍候…</p>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {preview && <div className="space-y-3 rounded-xl border border-primary/40 bg-primary/5 p-4" aria-label="导入预览">
        <h3 className="font-medium">确认导入：{preview.name}</h3>
        <p className="text-sm">找到 {preview.total} 个文档 · 已存在 {preview.duplicates} 个 · 跳过不支持文件 {preview.skipped} 个</p>
        <p className="text-sm text-muted-foreground">点击开始后先建立关键词索引，语义索引在启用模型后自动补建。</p>
        <ul className="max-h-48 space-y-1 overflow-auto text-sm">{preview.items?.slice(0, 100).map(item => <li key={item.id} className="break-all">{item.relative_path} · {(item.size / 1024).toFixed(1)} KiB{item.status === "duplicate" ? " · 已存在，跳过" : ""}</li>)}</ul>
        {preview.total > 100 && <p className="text-xs text-muted-foreground">预览显示前 100 个文件，共 {preview.total} 个。</p>}
        <div className="flex flex-wrap gap-2"><Button disabled={busy} onClick={() => void act(async () => { await learningAPI.startImport(preview.id); setPreview(null); toast.success("已开始批量入库，可以离开页面，后台会继续处理"); })}>开始批量入库</Button><Button variant="outline" disabled={busy} onClick={() => void act(async () => { await learningAPI.cancelImport(preview.id); setPreview(null); })}>取消本次导入</Button></div>
      </div>}
      {batches.map(batch => <div key={batch.id} className="space-y-3 rounded-lg border p-4">
        <div className="flex flex-wrap items-center justify-between gap-2"><p className="break-all font-medium">{batch.name}</p><span className="text-sm text-muted-foreground">{states[batch.status] ?? batch.status}{batch.failed > 0 ? " · 有文件失败" : ""}</span></div>
        <p className="text-sm">共 {batch.total} 个 · 可检索 {batch.ready} · 待处理 {batch.pending} · 失败 {batch.failed} · 重复 {batch.duplicates}{batch.cancelled ? ` · 已停止/删除 ${batch.cancelled}` : ""}{batch.skipped ? ` · 跳过其他文件 ${batch.skipped}` : ""}</p>
        {batch.status !== "preview" && <progress className="h-2 w-full" aria-label={`${batch.name}入库进度`} max={batch.total || 1} value={batch.total - batch.pending} />}
        <div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" disabled={busy} onClick={() => void act(() => inspect(batch))}>{batch.status === "preview" ? "查看清单并确认" : "查看文件与失败原因"}</Button>
          {(batch.status === "queued" || batch.status === "importing") && <Button size="sm" variant="outline" disabled={busy} onClick={() => void act(() => learningAPI.cancelImport(batch.id))}>停止待处理文件</Button>}
          {batch.status === "done" && batch.failed > 0 && <Button size="sm" variant="outline" disabled={busy} onClick={() => void act(() => learningAPI.retryImport(batch.id))}><RefreshCw className="size-4" />重试失败文件</Button>}
        </div>
        {details?.id === batch.id && <div className="space-y-2"><Input aria-label="筛选批次文件" value={filter} placeholder="按文件路径或错误筛选" onChange={e => setFilter(e.target.value)} /><div className="max-h-64 space-y-2 overflow-auto text-sm">{visible.slice(0, 100).map(item => <div key={item.id} className="break-all"><p>{item.relative_path} · {states[item.status] ?? item.status}</p>{item.error && <p className="text-destructive">{item.error}</p>}</div>)}</div>{visible.length > 100 && <p className="text-xs text-muted-foreground">共 {visible.length} 个匹配文件，显示前 100 个，请输入路径缩小范围。</p>}</div>}
      </div>)}
    </CardContent>
  </Card>;
}
