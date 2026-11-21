import { lazy, Suspense } from "react";

import { BrowserRouter, Navigate, Outlet, Route, Routes } from "react-router";

import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { PREFERENCE_DEFAULTS } from "@/lib/preferences/preferences-config";
import { PreferencesStoreProvider } from "@/stores/preferences/preferences-provider";

import MainLayout from "./app/(main)/layout";
import NotFound from "./app/not-found";

const Login = lazy(() => import("./app/(auth)/login/page"));
const Setup = lazy(() => import("./app/(auth)/setup/page"));
const PageChat = lazy(() => import("./app/(main)/chat/page"));
const PageDashboard = lazy(() => import("./app/(main)/dashboard/page"));
const PageFunctionAssets = lazy(() => import("./app/(main)/function/assets/page"));
const PageFunctionCommands = lazy(() => import("./app/(main)/function/commands/page"));
const PageFunctionSync = lazy(() => import("./app/(main)/function/sync/page"));
const PageFunctionFindingsDetail = lazy(() => import("./app/(main)/function/findings/detail/page"));
const PageFunctionFindings = lazy(() => import("./app/(main)/function/findings/page"));
const PageFunctionLlmRecords = lazy(() => import("./app/(main)/function/llm-records/page"));
const PageFunctionTasksDetail = lazy(() => import("./app/(main)/function/tasks/detail/page"));
const PageFunctionTasks = lazy(() => import("./app/(main)/function/tasks/page"));
const PageFunctionTraffic = lazy(() => import("./app/(main)/function/traffic/page"));
const PageFunctionWorkspace = lazy(() => import("./app/(main)/function/workspace/page"));
const PageSystemAgentsDetail = lazy(() => import("./app/(main)/system/agents/detail/page"));
const PageSystemAgents = lazy(() => import("./app/(main)/system/agents/page"));
const PageSystemInterceptApprovals = lazy(() => import("./app/(main)/system/intercept/approvals/page"));
const PageSystemInterceptAssets = lazy(() => import("./app/(main)/system/intercept/assets/page"));
const PageSystemIntercept = lazy(() => import("./app/(main)/system/intercept/page"));
const PageSystemLlm = lazy(() => import("./app/(main)/system/llm/page"));
const PageSystemLogs = lazy(() => import("./app/(main)/system/logs/page"));
const PageSystemMcp = lazy(() => import("./app/(main)/system/mcp/page"));
const PageSystemNotify = lazy(() => import("./app/(main)/system/notify/page"));
const PageSystemSettings = lazy(() => import("./app/(main)/system/settings/page"));
const PageSystemSkills = lazy(() => import("./app/(main)/system/skills/page"));
const PageSystemKnowledge = lazy(() => import("./app/(main)/system/knowledge/page"));
const PageSystemMemories = lazy(() => import("./app/(main)/system/memories/page"));
const PageSystemTools = lazy(() => import("./app/(main)/system/tools/page"));

export function App() {
  const defaults = PREFERENCE_DEFAULTS;
  return (
    <TooltipProvider>
      <PreferencesStoreProvider
        themeMode={defaults.theme_mode}
        themePreset={defaults.theme_preset}
        font={defaults.font}
        contentLayout={defaults.content_layout}
        navbarStyle={defaults.navbar_style}
      >
        <BrowserRouter>
          <Suspense
            fallback={
              <div role="status" className="p-6 text-sm text-muted-foreground">
                加载中…
              </div>
            }
          >
            <Routes>
              <Route path="/" element={<Navigate to="/function/tasks" replace />} />
              <Route path="/login" element={<Login />} />
              <Route path="/setup" element={<Setup />} />
              <Route
                element={
                  <MainLayout>
                    <Outlet />
                  </MainLayout>
                }
              >
                <Route path="/chat" element={<PageChat />} />
                <Route path="/dashboard" element={<PageDashboard />} />
                <Route path="/function/assets" element={<PageFunctionAssets />} />
                <Route path="/function/commands" element={<PageFunctionCommands />} />
                <Route path="/function/sync" element={<PageFunctionSync />} />
                <Route path="/function/findings/detail" element={<PageFunctionFindingsDetail />} />
                <Route path="/function/findings" element={<PageFunctionFindings />} />
                <Route path="/function/llm-records" element={<PageFunctionLlmRecords />} />
                <Route path="/function/tasks/detail" element={<PageFunctionTasksDetail />} />
                <Route path="/function/tasks" element={<PageFunctionTasks />} />
                <Route path="/function/traffic" element={<PageFunctionTraffic />} />
                <Route path="/function/workspace" element={<PageFunctionWorkspace />} />
                <Route path="/system/agents/detail" element={<PageSystemAgentsDetail />} />
                <Route path="/system/agents" element={<PageSystemAgents />} />
                <Route path="/system/intercept/approvals" element={<PageSystemInterceptApprovals />} />
                <Route path="/system/intercept/assets" element={<PageSystemInterceptAssets />} />
                <Route path="/system/intercept" element={<PageSystemIntercept />} />
                <Route path="/system/llm" element={<PageSystemLlm />} />
                <Route path="/system/logs" element={<PageSystemLogs />} />
                <Route path="/system/mcp" element={<PageSystemMcp />} />
                <Route path="/system/notify" element={<PageSystemNotify />} />
                <Route path="/system/settings" element={<PageSystemSettings />} />
                <Route path="/system/skills" element={<PageSystemSkills />} />
                <Route path="/system/retrieval" element={<Navigate to="/system/settings?tab=retrieval" replace />} />
                <Route path="/system/knowledge" element={<PageSystemKnowledge />} />
                <Route path="/system/memories" element={<PageSystemMemories />} />
                <Route path="/system/tools" element={<PageSystemTools />} />
              </Route>
              <Route path="*" element={<NotFound />} />
            </Routes>
          </Suspense>
        </BrowserRouter>
        <Toaster />
      </PreferencesStoreProvider>
    </TooltipProvider>
  );
}
