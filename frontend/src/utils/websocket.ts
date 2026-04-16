export type ConnectionStatus = 'connected' | 'disconnected' | 'connecting';

export interface ServerWebSocketMessage {
  type: string;
  payload: unknown;
}

export interface ClientWebSocketMessage {
  type: string;
  payload?: unknown;
}

const RECONNECT_DELAY_MS = 3000;
const MAX_OUTBOUND_QUEUE = 500;

let socket: WebSocket | null = null;
let reconnectTimeout: ReturnType<typeof setTimeout> | null = null;
let connectionStatus: ConnectionStatus = 'disconnected';
let shouldStayConnected = false;

const messageListeners = new Set<(message: ServerWebSocketMessage) => void>();
const statusListeners = new Set<(status: ConnectionStatus) => void>();
const outboundQueue: ClientWebSocketMessage[] = [];

function getWebSocketUrl(): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}/ws`;
}

function notifyStatus(nextStatus: ConnectionStatus) {
  connectionStatus = nextStatus;
  statusListeners.forEach((listener) => listener(nextStatus));
}

function clearReconnectTimer() {
  if (reconnectTimeout) {
    clearTimeout(reconnectTimeout);
    reconnectTimeout = null;
  }
}

function flushOutboundQueue() {
  if (!socket || socket.readyState !== WebSocket.OPEN) return;

  while (outboundQueue.length > 0) {
    const message = outboundQueue.shift();
    if (!message) continue;
    socket.send(JSON.stringify(message));
  }
}

function scheduleReconnect() {
  if (!shouldStayConnected || reconnectTimeout) return;

  reconnectTimeout = setTimeout(() => {
    reconnectTimeout = null;
    ensureWebSocketConnection();
  }, RECONNECT_DELAY_MS);
}

function cleanupIfIdle() {
  if (messageListeners.size > 0 || statusListeners.size > 0) return;
  if (outboundQueue.length > 0) return;

  shouldStayConnected = false;
  clearReconnectTimer();

  if (socket) {
    socket.close();
    socket = null;
  }

  notifyStatus('disconnected');
}

export function ensureWebSocketConnection() {
  shouldStayConnected = true;

  if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) {
    return;
  }

  clearReconnectTimer();
  notifyStatus('connecting');

  socket = new WebSocket(getWebSocketUrl());

  socket.onopen = () => {
    notifyStatus('connected');
    flushOutboundQueue();
  };

  socket.onmessage = (event) => {
    try {
      const parsed = JSON.parse(event.data) as ServerWebSocketMessage;
      messageListeners.forEach((listener) => listener(parsed));
    } catch (error) {
      console.error('[ws] Error parsing message:', error);
    }
  };

  socket.onerror = (error) => {
    console.error('[ws] WebSocket error:', error);
  };

  socket.onclose = () => {
    socket = null;
    notifyStatus('disconnected');
    scheduleReconnect();
  };
}

export function subscribeToWebSocketMessages(listener: (message: ServerWebSocketMessage) => void) {
  messageListeners.add(listener);
  ensureWebSocketConnection();

  return () => {
    messageListeners.delete(listener);
    cleanupIfIdle();
  };
}

export function subscribeToWebSocketStatus(listener: (status: ConnectionStatus) => void) {
  statusListeners.add(listener);
  listener(connectionStatus);
  ensureWebSocketConnection();

  return () => {
    statusListeners.delete(listener);
    cleanupIfIdle();
  };
}

export function sendWebSocketMessage(message: ClientWebSocketMessage) {
  if (outboundQueue.length >= MAX_OUTBOUND_QUEUE) {
    outboundQueue.shift();
  }

  outboundQueue.push(message);
  ensureWebSocketConnection();
  flushOutboundQueue();
}

export function __resetWebSocketClientForTests() {
  clearReconnectTimer();

  if (socket) {
    socket.close();
    socket = null;
  }

  outboundQueue.splice(0, outboundQueue.length);
  messageListeners.clear();
  statusListeners.clear();
  shouldStayConnected = false;
  connectionStatus = 'disconnected';
}
