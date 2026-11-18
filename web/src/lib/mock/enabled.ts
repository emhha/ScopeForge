// Mock 开关。构建期注入的公开变量（VITE_ 前缀才在浏览器可读）。
// Vercel 上设 VITE_MOCK=1 即整站走 mock、无需后端。
export const MOCK = import.meta.env.VITE_MOCK === "1";
