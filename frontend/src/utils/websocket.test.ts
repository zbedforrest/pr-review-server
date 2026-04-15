import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetWebSocketClientForTests,
  sendWebSocketMessage,
  subscribeToWebSocketMessages,
  subscribeToWebSocketStatus,
} from './websocket';

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];

  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;

  constructor(public readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(data: string) {
    this.sent.push(data);
  }

  close() {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.();
  }

  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }

  emitMessage(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) });
  }
}

describe('shared websocket client', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    __resetWebSocketClientForTests();
  });

  afterEach(() => {
    __resetWebSocketClientForTests();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('queues outbound messages until the socket opens', () => {
    sendWebSocketMessage({
      type: 'telemetry_batch',
      payload: { events: [{ action: 'search', label: 'abc' }] },
    });

    expect(MockWebSocket.instances).toHaveLength(1);
    expect(MockWebSocket.instances[0].sent).toHaveLength(0);

    MockWebSocket.instances[0].open();

    expect(MockWebSocket.instances[0].sent).toHaveLength(1);
    expect(JSON.parse(MockWebSocket.instances[0].sent[0])).toEqual({
      type: 'telemetry_batch',
      payload: { events: [{ action: 'search', label: 'abc' }] },
    });
  });

  it('flushes queued messages after reconnect', () => {
    const unsubscribe = subscribeToWebSocketStatus(() => {});

    expect(MockWebSocket.instances).toHaveLength(1);
    MockWebSocket.instances[0].open();
    MockWebSocket.instances[0].close();

    sendWebSocketMessage({
      type: 'telemetry_batch',
      payload: { events: [{ action: 'view_review' }] },
    });

    expect(MockWebSocket.instances).toHaveLength(2);
    expect(MockWebSocket.instances[1].sent).toHaveLength(0);

    MockWebSocket.instances[1].open();

    expect(MockWebSocket.instances[1].sent).toHaveLength(1);
    expect(JSON.parse(MockWebSocket.instances[1].sent[0])).toEqual({
      type: 'telemetry_batch',
      payload: { events: [{ action: 'view_review' }] },
    });

    unsubscribe();
  });

  it('delivers parsed inbound messages to subscribers', () => {
    const listener = vi.fn();
    const unsubscribe = subscribeToWebSocketMessages(listener);

    expect(MockWebSocket.instances).toHaveLength(1);
    MockWebSocket.instances[0].open();
    MockWebSocket.instances[0].emitMessage({
      type: 'pr_updated',
      payload: { owner: 'owner', repo: 'repo', number: 1 },
    });

    expect(listener).toHaveBeenCalledWith({
      type: 'pr_updated',
      payload: { owner: 'owner', repo: 'repo', number: 1 },
    });

    unsubscribe();
  });
});
