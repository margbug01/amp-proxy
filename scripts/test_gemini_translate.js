#!/usr/bin/env node
// End-to-end test for the Gemini translate path.
//
// Sends a Google v1beta1 generateContent request to a running amp-proxy
// instance (localhost:8317 by default) and verifies that the reply is a
// well-formed Gemini generateContent JSON body, i.e. that:
//
//   1. amp-proxy routed the request into the translate branch
//   2. customproxy forwarded a rewritten OpenAI Responses request to augment
//   3. augment returned a valid SSE reply
//   4. amp-proxy's ModifyResponse collapsed the SSE back into Gemini JSON
//
// Run:   node scripts/test_gemini_translate.js
// Env:   AMP_PROXY_URL   default http://127.0.0.1:8317
//        AMP_PROXY_KEY   default apikey (matches config.local.yaml)
//
// Exit code 0 on success, 1 on any failure. Errors are printed with enough
// detail to diagnose a broken translator without needing to re-enable
// bodyCapture.

const http = require('http');
const { URL } = require('url');

const PROXY_URL = process.env.AMP_PROXY_URL || 'http://127.0.0.1:8317';
const PROXY_KEY = process.env.AMP_PROXY_KEY || 'apikey';

// A minimal Gemini generateContent request that mirrors the finder
// subagent's request shape on the wire (contents + systemInstruction +
// tools + generationConfig). Tool parameter schemas use Gemini's
// uppercase type convention so we also exercise the schema case
// normalization path.
const REQUEST_BODY = {
  contents: [
    {
      role: 'user',
      parts: [
        { text: 'List the Go files in internal/customproxy. Use the glob tool.' },
      ],
    },
  ],
  systemInstruction: {
    role: 'user',
    parts: [
      {
        text: 'You are a fast, parallel code search agent. Respond with one tool call when asked.',
      },
    ],
  },
  tools: [
    {
      functionDeclarations: [
        {
          name: 'glob',
          description: 'Fast file pattern matching tool',
          parameters: {
            type: 'OBJECT',
            required: ['filePattern'],
            properties: {
              filePattern: { type: 'STRING', description: 'Glob pattern' },
              limit: { type: 'NUMBER', description: 'Max results' },
            },
          },
        },
      ],
    },
  ],
  generationConfig: {
    temperature: 1,
    maxOutputTokens: 8192,
    thinkingConfig: { includeThoughts: false, thinkingLevel: 'MINIMAL' },
  },
};

function postJSON(urlString, body) {
  return new Promise((resolve, reject) => {
    const url = new URL(urlString);
    const payload = Buffer.from(JSON.stringify(body));
    const req = http.request(
      {
        method: 'POST',
        hostname: url.hostname,
        port: url.port,
        path: url.pathname + url.search,
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': payload.length,
          'Authorization': `Bearer ${PROXY_KEY}`,
          'X-Goog-Api-Client': 'amp-proxy-test/0.1',
          'X-Amp-Feature': 'amp.chat',
          'X-Amp-Mode': 'smart',
          'X-Amp-Thread-Id': 'T-amp-proxy-test',
        },
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          resolve({
            status: res.statusCode,
            headers: res.headers,
            body: Buffer.concat(chunks).toString('utf8'),
          });
        });
      }
    );
    req.on('error', reject);
    req.write(payload);
    req.end();
  });
}

function assert(cond, message) {
  if (!cond) {
    console.error('  FAIL:', message);
    process.exitCode = 1;
    return false;
  }
  console.log('  PASS:', message);
  return true;
}

async function main() {
  const target = `${PROXY_URL}/api/provider/google/v1beta1/publishers/google/models/gemini-3-flash-preview:generateContent`;
  console.log('POST ->', target);

  let resp;
  try {
    resp = await postJSON(target, REQUEST_BODY);
  } catch (err) {
    console.error('  FAIL: network error —', err.message);
    process.exit(1);
  }

  console.log('status :', resp.status);
  console.log('ct     :', resp.headers['content-type']);
  console.log('bytes  :', resp.body.length);

  if (!assert(resp.status === 200, `HTTP 200 (got ${resp.status})`)) {
    console.error('---- response body ----');
    console.error(resp.body.slice(0, 2000));
    process.exit(1);
  }

  let parsed;
  try {
    parsed = JSON.parse(resp.body);
  } catch (err) {
    console.error('  FAIL: response is not JSON —', err.message);
    console.error(resp.body.slice(0, 2000));
    process.exit(1);
  }

  const ok =
    assert(Array.isArray(parsed.candidates) && parsed.candidates.length > 0, 'candidates[] non-empty') &&
    assert(parsed.candidates[0].content && Array.isArray(parsed.candidates[0].content.parts), 'candidates[0].content.parts array') &&
    assert(parsed.candidates[0].finishReason === 'STOP', `finishReason === "STOP" (got ${parsed.candidates[0].finishReason})`) &&
    assert(typeof parsed.usageMetadata === 'object' && parsed.usageMetadata !== null, 'usageMetadata object') &&
    assert(typeof parsed.usageMetadata.promptTokenCount === 'number', 'usageMetadata.promptTokenCount numeric') &&
    assert(typeof parsed.usageMetadata.candidatesTokenCount === 'number', 'usageMetadata.candidatesTokenCount numeric') &&
    assert(typeof parsed.modelVersion === 'string' && parsed.modelVersion.length > 0, 'modelVersion present');

  const parts = parsed.candidates[0].content.parts;
  console.log('\nparts summary (', parts.length, 'total):');
  parts.forEach((p, i) => {
    if (p.functionCall) {
      console.log(`  [${i}] functionCall name=${p.functionCall.name} args=${JSON.stringify(p.functionCall.args)}`);
    } else if (typeof p.text === 'string') {
      const preview = p.text.length > 120 ? p.text.slice(0, 120) + '…' : p.text;
      console.log(`  [${i}] text (${p.text.length} chars): ${JSON.stringify(preview)}`);
    } else {
      console.log(`  [${i}] unknown keys=${Object.keys(p).join(',')}`);
    }
  });

  console.log('\nusage:', JSON.stringify(parsed.usageMetadata));
  console.log('modelVersion:', parsed.modelVersion);
  console.log('responseId:', parsed.responseId);

  if (!ok) {
    console.error('\n---- raw response ----');
    console.error(resp.body.slice(0, 4000));
    process.exit(1);
  }

  console.log('\nOK — gemini translate end-to-end test passed');
}

main().catch((err) => {
  console.error('unhandled:', err);
  process.exit(1);
});
