import assert from 'node:assert/strict';
import { withFixture } from './fixture.mjs';

// The formatting toolbar's lists open where they can be seen (#94). They are
// children of the toolbar, and a backdrop filter on the toolbar kept them
// inside its 34 px box. The toolbar itself still scrolls sideways on a narrow
// screen, which is why its overflow stays as BlockNote sets it.
await withFixture(async ({ page, api, base, workspace }) => {
  page.setDefaultTimeout(15000);
  const doc = await api('POST', '/api/pages', { title: 'Toolbar', workspaceId: workspace.id });
  await api('PATCH', `/api/pages/${doc.id}`, {
    content: [
      { id: 'p1', type: 'paragraph', props: {}, content: [{ type: 'text', text: 'Select these words to format them', styles: {} }], children: [] },
    ],
  });
  const openTypeList = async () => {
    await page.goto(`${base}/p/${doc.id}`);
    const text = page.locator('.bn-block-outer[data-id="p1"] .bn-inline-content');
    await text.waitFor();
    await text.click({ clickCount: 3 });
    const toolbar = page.locator('.bn-formatting-toolbar');
    await toolbar.waitFor();
    await toolbar.locator('button').first().click();
    const list = page.locator('.bn-formatting-toolbar .mantine-Menu-dropdown');
    await list.waitFor();
    return { toolbar, list };
  };
  const seen = (list) =>
    list.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + 12);
      return !!hit && el.contains(hit);
    });

  const { list } = await openTypeList();
  assert.equal(await seen(list), true, 'the block type list must be visible where it opened');
  await list.getByText('Heading 1', { exact: true }).click();
  await page.locator('.bn-block-outer[data-id="p1"] h1').waitFor();

  // On a phone the toolbar is wider than the screen and still scrolls.
  await page.setViewportSize({ width: 375, height: 800 });
  const narrow = await openTypeList();
  const scrolls = await narrow.toolbar.evaluate((el) => ({
    overflow: getComputedStyle(el).overflowX,
    wider: el.scrollWidth > el.clientWidth,
  }));
  assert.equal(scrolls.overflow, 'auto');
  assert.equal(scrolls.wider, true, 'at 375 px the toolbar should be wider than its box');
  assert.equal(await seen(narrow.list), true, 'the list must be visible on a phone too');

  console.log('Toolbar menus: the block type list opens in sight, and the toolbar still scrolls on a phone.');
});
