'use strict';

// Exercise the real playersSpark function from index.html; no browser or dependencies.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../app/static/index.html'), 'utf8');
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(match => match[1]).join('\n');
const source = /function playersSpark\(buckets\) \{[\s\S]*?\n\}/.exec(scripts);
assert.ok(source, 'playersSpark must exist in index.html');

const ctx = vm.createContext({});
vm.runInContext(`${source[0]}; this.playersSpark = playersSpark;`, ctx);
const spark = buckets => vm.runInContext('playersSpark', ctx)(buckets);

const buckets = values => values.map((v, i) => ({ ts: 1700000000 + i * 300, players_avg: v }));
const polygons = out => [...out.matchAll(/<polygon points="([^"]+)"/g)].map(m => m[1]);
const polylines = out => [...out.matchAll(/<polyline points="([^"]+)"/g)].map(m => m[1]);
const circles = out => [...out.matchAll(/<circle cx="([^"]+)" cy="([^"]+)" r="([^"]+)"/g)];

test('channels without player data render nothing at all', () => {
  assert.equal(spark(buckets([null, null, null])), '');
  assert.equal(spark([]), '');
  assert.equal(spark(buckets([undefined, null])), '');
});

test('contiguous samples produce one line and one filled area', () => {
  const out = spark(buckets([2, 4, 8]));
  assert.equal(polygons(out).length, 1);
  assert.equal(polylines(out).length, 1);
  assert.equal(circles(out).length, 0);
  assert.match(out, /峰值 8 人/);
});

test('gaps split the line and never fill across missing buckets', () => {
  const out = spark(buckets([2, 4, null, 6, 8, null, null, 3]));
  // Three runs of data: [2,4], [6,8], [3]. The last is a single point, drawn as a dot.
  assert.equal(polygons(out).length, 3);
  assert.equal(polylines(out).length, 2);
  assert.equal(circles(out).length, 1);
  const width = 900;
  const step = width / 7;
  const xs = polygons(out).map(points => {
    const nums = points.split(' ').map(pair => Number(pair.split(',')[0]));
    return [Math.min(...nums), Math.max(...nums)];
  });
  assert.deepEqual(xs[0], [0, Number((step).toFixed(1))]);
  assert.deepEqual(xs[1], [Number((3 * step).toFixed(1)), Number((4 * step).toFixed(1))]);
  assert.deepEqual(xs[2], [Number((7 * step).toFixed(1)), Number((7 * step).toFixed(1))]);
  // No area may span the whole chart, which is what a cross-gap fill would do.
  assert.ok(xs.every(([left, right]) => right - left < width));
});

test('an isolated sample is drawn as a dot instead of an invisible polyline', () => {
  const out = spark(buckets([5]));
  assert.equal(polylines(out).length, 0);
  assert.equal(circles(out).length, 1);
  assert.match(out, /峰值 5 人/);

  const isolated = spark(buckets([null, null, 7, null]));
  assert.equal(polylines(isolated).length, 0);
  assert.equal(circles(isolated).length, 1);
  // Peak 7 maps to the top inset, so its vertical centre is pad = 2.
  assert.equal(circles(isolated)[0][2], '2.0');
});

test('a real zero is plotted while a missing bucket is skipped', () => {
  assert.notEqual(spark(buckets([0])), '', 'a zero population is data, not a gap');
  const out = spark(buckets([0, 5]));
  assert.equal(polylines(out).length, 1);
  assert.equal(polygons(out).length, 1);
  // Zero sits on the inset baseline (height - pad = 26), a nonzero value sits above it.
  const ys = polylines(out)[0].split(' ').map(pair => Number(pair.split(',')[1]));
  assert.equal(ys[0], 26);
  assert.ok(ys[1] < 26);
  assert.match(out, /峰值 5 人/);
  assert.doesNotMatch(out, /NaN|Infinity|undefined|null/);
});

test('thin slices without player keys are ignored and peak comes from averages', () => {
  const out = spark([{ ts: 1, status: 'ok' }, { ts: 2, status: 'down' }]);
  assert.equal(out, '');
  const shown = spark(buckets([3, 3, 3, 3]));
  assert.match(shown, /峰值 3 人/);
  assert.match(shown, /最近 3 人/);
});
