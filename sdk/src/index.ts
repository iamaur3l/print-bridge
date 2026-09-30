/**
 * PrintBridge Web SDK - Client Library for PrintBridge Local Agent
 */

export const VERSION = "0.1.0";

export type EventCallback = (data: any) => void;

/**
 * Machine-readable failure reasons. Callers can now distinguish "the agent is not
 * installed/running" from "the browser blocked access to the local network" from
 * "we asked for a response and never got one".
 */
export type PrintBridgeErrorCode =
  | 'not_connected'
  | 'timeout'
  | 'permission_denied'
  | 'pairing_required'
  | 'agent_absent'
  | 'request_failed';

export class PrintBridgeError extends Error {
  public readonly code: PrintBridgeErrorCode;

  constructor(code: PrintBridgeErrorCode, message: string) {
    super(message);
    this.name = 'PrintBridgeError';
    this.code = code;
  }
}

/**
 * Chrome 142+ gates fetch/XHR to localhost behind "Local Network Access", and
 * Chrome 147+ extends the same permission to WebSocket connections.
 * https://developer.chrome.com/blog/local-network-access
 */
export type LocalNetworkPermission = 'granted' | 'prompt' | 'denied' | 'unsupported';

export interface PrintBridgeConfig {
  host?: string;
  port?: number;
  autoConnect?: boolean;
  token?: string;
  reconnectInterval?: number;
  /** Use wss:// instead of ws://. Defaults to true when the page itself is served over HTTPS. */
  secure?: boolean;
  /** How long to wait for a response before rejecting, in ms. Default 15000. */
  requestTimeout?: number;
  /** Upper bound for reconnect backoff, in ms. Default 30000. */
  maxReconnectDelay?: number;
}

export interface PrinterInfo {
  name: string;
  driver_name: string;
  port_name: string;
  is_default: boolean;
  is_online: boolean;
  /**
   * Corroborated verdict: 'online' | 'offline' | 'unknown'.
   * 'unknown' means the spooler reported offline but nothing corroborated it
   * (a stale WorkOffline flag on a working USB printer), so it must not be shown
   * to the user as a broken printer.
   */
  state?: string;
  /** Human-readable label for `state`. */
  status_description?: string;
  /** How the verdict was reached (e.g. "a raw print completed 4s ago"). */
  status_detail?: string;
  /** True when the spooler's offline flag was contradicted by evidence. */
  stale_work_offline?: boolean;
  /**
   * 'local' when the job goes through the OS print queue, 'network' when it is
   * written straight to the printer's TCP RAW port (9100 by default).
   */
  type?: string;
  /** 'host:port' for network printers. */
  network_address?: string;
}

export interface PrintOptions {
  jobName?: string;
  format?: 'raw' | 'escpos' | 'zpl';
  /** Address a station role ("kitchen") instead of a device name. */
  role?: string;
  /** Higher runs sooner; the queue is ordered by priority then arrival. */
  priority?: number;
  /**
   * Makes enqueueing safe to retry: a repeat with the same key returns the original
   * job instead of printing a second receipt.
   */
  idempotencyKey?: string;
}

/** The result of enqueueing a print job. */
export interface PrintResult {
  jobId: string;
  /** True when the job was reused via its idempotency key rather than created. */
  duplicate: boolean;
}

/** Queue depth by status. */
export interface QueueCounts {
  queued: number;
  sending: number;
  success: number;
  failed: number;
  retrying: number;
  total: number;
}

/** Queue state as reported by queue_status. */
export interface QueueStatus {
  paused: boolean;
  counts: QueueCounts;
}

/** What a physical printer can do when it serves a role. */
export interface PrinterCapabilities {
  max_width: number;
  supports_bold: boolean;
  supports_underline: boolean;
  supports_barcode: boolean;
  supports_qr_code: boolean;
  supports_image: boolean;
  supports_cut: boolean;
  supports_partial_cut: boolean;
  supports_cash_drawer: boolean;
  codepage: number;
}

/** A station role plus how it currently resolves. */
export interface PrinterRoleStatus {
  role: string;
  printer_name: string;
  label?: string;
  capabilities: PrinterCapabilities;
  /** False when the assigned printer is not installed any more. */
  resolved: boolean;
  state?: string;
  status?: string;
  detail?: string;
  reason?: string;
}

/** A plain-language printer diagnosis. */
export interface PrinterDiagnosis {
  printer: string;
  installed: boolean;
  type?: string;
  port_name?: string;
  network_address?: string;
  state?: string;
  status_description?: string;
  /** Checks written to be read by whoever is standing at the till. */
  checks: string[];
  roles?: string[];
}

export interface JobStatus {
  id: string;
  printer_name: string;
  job_name: string;
  status: 'queued' | 'sending' | 'success' | 'failed' | 'retrying';
  created_at: string;
  attempts: number;
  error_message?: string;
}

export class PrintBridgeClient {
  private host: string;
  private port: number;
  private secure: boolean;
  private token: string;
  private ws: WebSocket | null = null;
  private listeners: Map<string, Set<EventCallback>> = new Map();
  private pendingRequests: Map<string, { resolve: (val: any) => void; reject: (err: any) => void; timer: any }> = new Map();
  private reqCounter = 1;
  private reconnectInterval: number;
  private maxReconnectDelay: number;
  private requestTimeout: number;
  private isConnected = false;
  private connecting = false;
  private manualClose = false;
  private reconnectAttempts = 0;
  private reconnectTimer: any = null;

  constructor(config: PrintBridgeConfig = {}) {
    this.host = config.host || 'localhost';
    this.port = config.port || 9567;
    // An HTTPS page must use wss://, and Chrome 147+ additionally gates
    // local-network WebSockets behind the Local Network Access permission.
    this.secure = config.secure !== undefined
      ? config.secure
      : (typeof location !== 'undefined' && location.protocol === 'https:');
    this.token = config.token || (typeof localStorage !== 'undefined' ? localStorage.getItem('printbridge_token') || '' : '');
    this.reconnectInterval = config.reconnectInterval || 3000;
    this.maxReconnectDelay = config.maxReconnectDelay || 30000;
    this.requestTimeout = config.requestTimeout || 15000;

    if (config.autoConnect !== false && typeof window !== 'undefined') {
      this.connect().catch(() => {});
    }
  }

  /** The WebSocket URL this client talks to. */
  public get url(): string {
    return `${this.secure ? 'wss' : 'ws'}://${this.host}:${this.port}/ws`;
  }

  public get connected(): boolean {
    return this.isConnected;
  }

  public connect(): Promise<void> {
    // Guard against concurrent connects (autoConnect plus an explicit call).
    if (this.connecting) {
      return Promise.resolve();
    }
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) {
      return Promise.resolve();
    }

    this.manualClose = false;

    return new Promise((resolve, reject) => {
      if (typeof WebSocket === 'undefined') {
        reject(new PrintBridgeError('agent_absent', 'WebSocket is not available in this environment'));
        return;
      }

      this.connecting = true;

      let socket: WebSocket;
      try {
        socket = new WebSocket(this.url);
      } catch (err) {
        this.connecting = false;
        reject(new PrintBridgeError('agent_absent', `Could not open ${this.url}: ${String(err)}`));
        return;
      }
      this.ws = socket;

      socket.onopen = () => {
        this.connecting = false;
        this.isConnected = true;
        this.reconnectAttempts = 0;
        this.emit('connected', { host: this.host, port: this.port, url: this.url });
        if (this.token) {
          this.authenticate(this.token).catch(() => {});
        }
        resolve();
      };

      socket.onclose = () => {
        const reachedAgent = this.isConnected;
        this.isConnected = false;
        this.connecting = false;

        this.rejectAllPending(new PrintBridgeError('not_connected', 'Connection closed before a response arrived'));
        this.emit('disconnected', {});

        if (this.manualClose) {
          return;
        }
        if (!reachedAgent) {
          this.emit('error', new PrintBridgeError('agent_absent', `PrintBridge Agent is not reachable at ${this.url}`));
        }
        this.scheduleReconnect();
      };

      socket.onerror = () => {
        if (this.connecting || this.isConnected) {
          this.emit('error', new PrintBridgeError('agent_absent', `Failed to reach PrintBridge Agent at ${this.url}`));
        }
      };

      socket.onmessage = (event) => {
        try {
          this.handleIncomingMessage(JSON.parse(event.data));
        } catch (e) {
          console.error('[PrintBridge SDK] Failed to parse message:', e);
        }
      };
    });
  }

  public disconnect(): void {
    // Without this flag the socket's own close handler would immediately
    // schedule a reconnect and the client could never stay disconnected.
    this.manualClose = true;
    this.rejectAllPending(new PrintBridgeError('not_connected', 'Client disconnected'));

    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
    this.isConnected = false;
    this.connecting = false;
  }

  private scheduleReconnect(): void {
    if (this.reconnectTimer !== null) {
      return;
    }
    const delay = Math.min(this.reconnectInterval * Math.pow(2, this.reconnectAttempts), this.maxReconnectDelay);
    this.reconnectAttempts += 1;

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect().catch(() => {});
    }, delay);
  }

  private rejectAllPending(error: PrintBridgeError): void {
    this.pendingRequests.forEach(({ reject, timer }) => {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      reject(error);
    });
    this.pendingRequests.clear();
  }

  // --- Sub-APIs ---

  public pairing = {
    /**
     * Asks the agent for a pairing code. The code is deliberately NOT returned
     * here: it appears in the agent tray icon and on the dashboard
     * (http://localhost:9567/dashboard) for the human to read and type in.
     */
    request: async (appName: string = 'Web Application'): Promise<{ code_requested: boolean; expires_at: string }> => {
      return this.sendRequest('request_pairing', { app_name: appName });
    },
    confirm: async (code: string) => {
      const resp = await this.sendRequest('confirm_pairing', { code });
      if (resp.token) {
        this.pairing.setToken(resp.token);
      }
      return resp;
    },
    setToken: (token: string) => {
      this.token = token;
      if (typeof localStorage !== 'undefined') {
        localStorage.setItem('printbridge_token', token);
      }
    },
    getToken: () => this.token,
    isPaired: () => Boolean(this.token)
  };

  public permissions = {
    /**
     * Chrome 142+ gates fetch/XHR to localhost behind Local Network Access, and
     * Chrome 147+ extends the same permission to WebSocket connections.
     * Firefox and Safari do not enforce it (yet).
     */
    localNetwork: async (): Promise<LocalNetworkPermission> => {
      if (typeof navigator === 'undefined' || !navigator.permissions || typeof navigator.permissions.query !== 'function') {
        return 'unsupported';
      }
      try {
        const status = await (navigator.permissions as any).query({ name: 'local-network-access' });
        return status.state as LocalNetworkPermission;
      } catch {
        // The permission is unknown to this browser.
        return 'unsupported';
      }
    }
  };

  /**
   * Explains why a connection is failing. "Agent not installed", "browser
   * permission blocked" and "wrong scheme/port" all look identical as a bare
   * failed WebSocket, so surface this in your UI instead of guessing.
   */
  public async diagnose(): Promise<{
    url: string;
    connected: boolean;
    secure: boolean;
    localNetworkPermission: LocalNetworkPermission;
    hint: string;
  }> {
    const permission = await this.permissions.localNetwork();

    let hint: string;
    if (this.isConnected) {
      hint = 'Connected to the PrintBridge Agent.';
    } else if (permission === 'denied') {
      hint = 'Local Network Access is blocked for this origin. Re-enable it from the address bar (Site settings -> Local Network, or "Apps on device" on Chrome 145+).';
    } else if (permission === 'prompt') {
      hint = 'The browser will ask for Local Network Access permission - tell the user to choose Allow.';
    } else if (this.secure) {
      hint = 'This client is using wss://. Start the agent with -tls-cert/-tls-key, or use { secure: false } for a local ws:// connection.';
    } else {
      hint = 'Is the PrintBridge Agent running? Check the tray icon or open http://localhost:9567/dashboard.';
    }

    return {
      url: this.url,
      connected: this.isConnected,
      secure: this.secure,
      localNetworkPermission: permission,
      hint
    };
  }

  public printers = {
    find: async (): Promise<PrinterInfo[]> => {
      const resp = await this.sendRequest('list_printers');
      return resp.printers || [];
    },
    health: async (): Promise<PrinterInfo[]> => {
      const resp = await this.sendRequest('get_printer_health');
      return resp.printers || [];
    }
  };

  public jobs = {
    status: async (jobId: string): Promise<JobStatus> => {
      return this.sendRequest('job_status', { job_id: jobId });
    }
  };

  public devices = {
    openDrawer: (printerName: string, options: { pin?: 2 | 5; brand?: string } = {}) => {
      return this.sendRequest('open_drawer', {
        printer: printerName,
        pin: options.pin || 2,
        brand: options.brand || 'epson'
      });
    },
    listSerialPorts: () => this.sendRequest('list_serial_ports'),
    listHIDDevices: () => this.sendRequest('list_hid_devices')
  };

  /**
   * Encodes receipt bytes as base64. Strings are UTF-8 encoded (what callers mean
   * for ESC/POS text) instead of being mangled through the deprecated unescape().
   */
  private encodeBase64(data: string | Uint8Array): string {
    let bytes: Uint8Array;
    if (typeof data === 'string') {
      if (typeof TextEncoder !== 'undefined') {
        bytes = new TextEncoder().encode(data);
      } else {
        bytes = new Uint8Array(data.length);
        for (let i = 0; i < data.length; i++) {
          bytes[i] = data.charCodeAt(i) & 0xff;
        }
      }
    } else {
      bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
    }

    if (typeof btoa !== 'function') {
      throw new PrintBridgeError('request_failed', 'No base64 encoder is available in this environment');
    }

    let binary = '';
    for (let i = 0; i < bytes.length; i++) {
      binary += String.fromCharCode(bytes[i]);
    }
    return btoa(binary);
  }

  public async print(printerName: string, data: string | Uint8Array, options: PrintOptions = {}): Promise<PrintResult> {
    const base64Data = this.encodeBase64(data);

    const payload: any = {
      data: base64Data,
      job_name: options.jobName || 'PrintBridge SDK Print Job'
    };

    // A job addressed to a role follows whatever device currently serves it; the
    // agent resolves the role at enqueue time and records the resolved device.
    if (options.role) {
      payload.role = options.role;
    } else {
      payload.printer = printerName;
    }

    if (options.priority) {
      payload.priority = options.priority;
    }
    if (options.idempotencyKey) {
      payload.idempotency_key = options.idempotencyKey;
    }

    if (this.token) {
      payload.token = this.token;
    }

    const resp = await this.sendRequest('print', payload);
    return { jobId: resp.job_id, duplicate: Boolean(resp.duplicate) };
  }

  /** Queue depth, pause state, and the operator actions around a stuck job. */
  public queue = {
    status: (): Promise<QueueStatus> => this.sendRequest('queue_status'),
    pause: () => this.sendRequest('pause_queue'),
    resume: () => this.sendRequest('resume_queue'),
    retryJob: (jobId: string) => this.sendRequest('retry_job', { job_id: jobId }),
    clearFailed: () => this.sendRequest('clear_failed_jobs')
  };

  /**
   * Prints to a station role ("kitchen") rather than a device name, so replacing the
   * hardware behind a station never requires a change in the POS.
   */
  public async printToRole(role: string, data: string | Uint8Array, options: PrintOptions = {}): Promise<PrintResult> {
    return this.print(role, data, { ...options, role });
  }

  /** Station roles: list, assign (or reassign) and remove. */
  public roles = {
    list: async (): Promise<PrinterRoleStatus[]> => {
      const resp = await this.sendRequest('list_printer_roles');
      return resp.roles || [];
    },
    assign: (
      role: string,
      printerName: string,
      options: { label?: string; capabilities?: Partial<PrinterCapabilities> } = {}
    ) => this.sendRequest('assign_printer_role', {
      role,
      printer: printerName,
      label: options.label,
      capabilities: options.capabilities
    }),
    remove: (role: string) => this.sendRequest('remove_printer_role', { role })
  };

  /** Sends a built-in ESC/POS test slip to a printer or a role. */
  public testPrint(target: { printer?: string; role?: string; slip?: 'receipt' | 'kitchen' | 'text' } = {}): Promise<any> {
    return this.sendRequest('test_print', target);
  }

  /** Explains why a printer or role is not working, in plain language. */
  public diagnosePrinter(target: { printer?: string; role?: string } = {}): Promise<PrinterDiagnosis> {
    return this.sendRequest('diagnose_printer', target);
  }

  public authenticate(token?: string): Promise<any> {
    const tok = token || this.token;
    return this.sendRequest('authenticate', { token: tok });
  }

  // --- Event Emitter ---

  public on(event: string, callback: EventCallback): void {
    if (!this.listeners.has(event)) {
      this.listeners.set(event, new Set());
    }
    this.listeners.get(event)!.add(callback);
  }

  public off(event: string, callback: EventCallback): void {
    if (this.listeners.has(event)) {
      this.listeners.get(event)!.delete(callback);
    }
  }

  private emit(event: string, data: any): void {
    if (this.listeners.has(event)) {
      this.listeners.get(event)!.forEach((cb) => {
        try {
          cb(data);
        } catch (e) {
          console.error('[PrintBridge SDK] Error in event listener:', e);
        }
      });
    }
  }

  private handleIncomingMessage(msg: any): void {
    if (msg.id && this.pendingRequests.has(msg.id)) {
      const { resolve, reject, timer } = this.pendingRequests.get(msg.id)!;
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      this.pendingRequests.delete(msg.id);

      if (msg.success) {
        resolve(msg.payload);
      } else {
        const code: PrintBridgeErrorCode = msg.error === 'pairing_required' ? 'pairing_required' : 'request_failed';
        reject(new PrintBridgeError(code, msg.error || 'Request failed'));
      }
    }

    if (msg.type === 'job_status_changed') {
      this.emit('job_status_changed', msg.payload);
    } else if (msg.type === 'printer_status_changed') {
      this.emit('printer_status_changed', msg.payload);
    }
  }

  private sendRequest(type: string, payload: any = {}): Promise<any> {
    return new Promise((resolve, reject) => {
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
        reject(new PrintBridgeError('not_connected', `Cannot send "${type}": not connected to ${this.url}`));
        return;
      }

      const id = `sdk_${this.reqCounter++}`;

      // Without a timeout a request that is never answered leaks its promise
      // forever, which is how "nothing happens when I click print" happens.
      const timer = setTimeout(() => {
        this.pendingRequests.delete(id);
        reject(new PrintBridgeError('timeout', `No response to "${type}" within ${this.requestTimeout}ms`));
      }, this.requestTimeout);

      this.pendingRequests.set(id, { resolve, reject, timer });

      const msg = {
        version: 1,
        id,
        type,
        payload
      };

      try {
        this.ws.send(JSON.stringify(msg));
      } catch (err) {
        clearTimeout(timer);
        this.pendingRequests.delete(id);
        reject(new PrintBridgeError('request_failed', `Failed to send "${type}": ${String(err)}`));
      }
    });
  }
}

export default PrintBridgeClient;
