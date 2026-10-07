/**
 * 应用入口。
 *
 * 只负责挂载，逻辑全在 App.tsx 里。
 *
 * ★ `#boot` 占位元素必须在挂载后移除：
 *   index.html 里有一段纯 CSS 的首屏骨架（不依赖任何 JS chunk），
 *   作用是让用户在 bundle 下载期间看到结构而不是纯白屏。
 *   如果 React 挂载失败，这段骨架会**一直留着**（这正是它的价值 —— 不会白屏），
 *   所以挂载成功后要显式移除。
 */

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import './index.css';

const container = document.getElementById('root');
if (!container) {
  throw new Error('缺少 #root 容器：index.html 可能被改动过');
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);

// 移除首屏占位（React 已接管 DOM）。
const boot = document.getElementById('boot');
if (boot) boot.remove();