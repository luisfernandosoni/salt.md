import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { eventually, withFixture } from './fixture.mjs';

// A client that asks for no scope in particular, as Claude Code's
// `claude mcp add --transport http` does, got read access with no way to say
// more on the consent screen (#83). The screen offers both now, starts on what
// was asked for, and the token answer names what was granted.
await withFixture(async ({ page, context, base }) => {
  page.setDefaultTimeout(15000);
  const redirect = `${base}/oauth-test-callback`;
  const client = await (
    await context.request.post(`${base}/oauth/register`, {
      data: { client_name: 'Consent test', redirect_uris: [redirect] },
    })
  ).json();
  const verifier = randomBytes(32).toString('base64url');
  const challenge = createHash('sha256').update(verifier).digest('base64url');
  const authorize =
    `${base}/oauth/authorize?` +
    new URLSearchParams({
      response_type: 'code',
      client_id: client.client_id,
      redirect_uri: redirect,
      code_challenge: challenge,
      code_challenge_method: 'S256',
      state: 'xyz',
    });
  // The callback is the client's address, not a page of ours: keep the code
  // from the address as the browser arrives there. (Routing the request does
  // not work: the app's service worker answers it first.)
  let code = '';
  page.on('framenavigated', (frame) => {
    if (frame === page.mainFrame() && frame.url().startsWith(redirect)) {
      code = new URL(frame.url()).searchParams.get('code') ?? '';
    }
  });
  await page.goto(authorize);
  const read = page.getByRole('radio', { name: 'read pages', exact: true });
  const write = page.getByRole('radio', { name: 'read and change pages', exact: true });
  await read.waitFor();
  assert.equal(await read.isChecked(), true, 'a request without a scope starts on read');
  await write.check();
  await page.getByRole('radio', { name: /Every workspace/ }).check();
  await page.getByRole('button', { name: 'Allow', exact: true }).click();
  await eventually(async () => code !== '');

  const token = await (
    await context.request.post(`${base}/oauth/token`, {
      form: {
        grant_type: 'authorization_code',
        code,
        client_id: client.client_id,
        redirect_uri: redirect,
        code_verifier: verifier,
      },
    })
  ).json();
  assert.equal(token.scope, 'write', 'the token answer names what was granted');

  // And the agent can write with it.
  const answer = await (
    await context.request.post(`${base}/mcp`, {
      headers: { Authorization: `Bearer ${token.access_token}` },
      data: {
        jsonrpc: '2.0',
        id: 1,
        method: 'tools/call',
        params: { name: 'create_page', arguments: { title: 'Written by the agent' } },
      },
    })
  ).json();
  assert(!answer.error && !answer.result?.isError, JSON.stringify(answer).slice(0, 300));

  console.log('OAuth consent: the person picks read and write, and the agent can write.');
});
