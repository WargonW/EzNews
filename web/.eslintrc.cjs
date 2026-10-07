/**
 * ESLint 配置（ESLint 8.x 的 .eslintrc.cjs 格式）。
 *
 * 刻意保持精简：
 * - 类型相关的错误交给 `tsc --noEmit`（唯一权威），这里不重复做类型检查；
 * - 零 warning（--max-warnings 0），所以规则按"error"级别设。
 */
module.exports = {
  root: true,
  env: { browser: true, es2022: true },
  parser: '@typescript-eslint/parser',
  parserOptions: {
    ecmaVersion: 'latest',
    sourceType: 'module',
    ecmaFeatures: { jsx: true },
  },
  plugins: ['@typescript-eslint', 'react-hooks', 'react-refresh'],
  extends: [
    'eslint:recommended',
    'plugin:@typescript-eslint/recommended',
    // react-hooks 的两条核心规则：
    // - rules-of-hooks：hook 调用位置违规
    // - exhaustive-deps：依赖数组漏项（这类 bug 极难靠肉眼发现）
    'plugin:react-hooks/recommended',
  ],
  settings: {
    react: { version: '18.3' },
  },
  ignorePatterns: ['dist', 'node_modules', '*.cjs', '.eslintrc.cjs'],
  rules: {
    // 构建产物里 console 是噪音；但业务代码里误留 console 会暴露内部信息，
    // 所以这里改成 warn 并配合 --max-warnings 0 → 实际上等同禁止。
    'no-console': 'warn',
    // 未使用变量：TS 的 noUnusedLocals 已覆盖大部分，但 eslint 侧对
    // 解构剩余项、函数参数更敏感，补一条。
    '@typescript-eslint/no-unused-vars': [
      'error',
      { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
    ],
    // 未使用变量必须显式忽略时用 `_` 前缀（已在上面配置）。
    'no-empty': ['error', { allowEmptyCatch: true }],
    eqeqeq: ['error', 'smart'],
    'prefer-const': 'error',
    'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
  },
  overrides: [
    {
      // vite.config.ts / *.cjs 跑在 Node 环境
      files: ['*.config.ts', '*.config.js'],
      env: { node: true },
      rules: { 'no-console': 'off' },
    },
  ],
};