#!/usr/bin/env node
// 最小 MCP stdio echo 服务器(测试 fixture,协议版本 2024-11-05)
// 换行分隔 JSON-RPC 2.0。
'use strict';
const readline = require('readline');

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });

function respond(id, result) {
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n');
}

function respondError(id, code, message) {
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }) + '\n');
}

rl.on('line', (line) => {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }
  if (!msg.method) return;
  switch (msg.method) {
    case 'initialize':
      respond(msg.id, {
        protocolVersion: '2024-11-05',
        capabilities: { tools: {} },
        serverInfo: { name: 'echo-server', version: '1.0.0' },
      });
      break;
    case 'notifications/initialized':
      break;
    case 'tools/list':
      respond(msg.id, {
        tools: [
          {
            name: 'echo',
            description: '回显输入参数',
            inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
          },
          {
            name: 'add',
            description: '两数相加',
            inputSchema: { type: 'object', properties: { a: { type: 'number' }, b: { type: 'number' } } },
          },
        ],
      });
      break;
    case 'tools/call': {
      const { name, arguments: args } = msg.params;
      if (name === 'echo') {
        respond(msg.id, { content: [{ type: 'text', text: 'echo: ' + (args && args.text || '') }] });
      } else if (name === 'add') {
        const sum = (args.a || 0) + (args.b || 0);
        respond(msg.id, { content: [{ type: 'text', text: String(sum) }] });
      } else {
        respondError(msg.id, -32602, 'unknown tool: ' + name);
      }
      break;
    }
    default:
      respondError(msg.id, -32601, 'method not found: ' + msg.method);
  }
});
