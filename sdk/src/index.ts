/**
 * PrintBridge Web SDK - Client Library for PrintBridge Local Agent
 */

export const VERSION = "0.1.0";

export type EventCallback = (data: any) => void;

export interface PrintBridgeConfig {
  host?: string;
  port?: number;
  autoConnect?: boolean;
  token?: string;
  reconnectInterval?: number;
}

export interface PrinterInfo {
  name: string;
  driver_name: string;
  port_name: string;
  is_default: boolean;
  is_online: boolean;
  status_description?: string;
}

export interface PrintOptions {
  jobName?: string;
  format?: 'raw' | 'escpos' | 'zpl';
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
  private token: string;
  private ws: WebSocket | null = null;
  private listeners: Map<string, Set<EventCallback>> = new Map();
  private pendingRequests: Map<string, { resolve: (val: any) => void; reject: (err: any) => void }> = new Map();
  private reqCounter = 1;
  private reconnectInterval: number;
  private isConnected = false;

  constructor(config: PrintBridgeConfig = {}) {
    this.host = config.host || 'localhost';
    this.port = config.port || 9567;
    this.token = config.token || (typeof localStorage !== 'undefined' ? localStorage.getItem('printbridge_token') || '' : '');
    this.reconnectInterval = config.reconnectInterval || 3000;

    if (config.autoConnect !== false && typeof window !== 'undefined') {
      this.connect().catch(() => {});
    }
  }

  public connect(): Promise<void> {
    return new Promise((resolve, reject) => {
      const url = `ws://${this.host}:${this.port}/ws`;
      this.ws = new WebSocket(url);

      this.ws.onopen = () => {
        this.isConnected = true;
        this.emit('connected', { host: this.host, port: this.port });
        if (this.token) {
          this.authenticate(this.token).catch(() => {});
        }
        resolve();
      };

      this.ws.onclose = () => {
        this.isConnected = false;
        this.emit('disconnected', {});
        setTimeout(() => {
          if (!this.isConnected) {
            this.connect().catch(() => {});
          }
        }, this.reconnectInterval);
      };

      this.ws.onerror = (err) => {
        this.emit('error', err);
        if (!this.isConnected) {
          reject(err);
        }
      };

      this.ws.onmessage = (event) => {
        try {
          const msg = JSON.parse(event.data);
          this.handleIncomingMessage(msg);
        } catch (e) {
          console.error('[PrintBridge SDK] Failed to parse message:', e);
        }
      };
    });
  }

  public disconnect(): void {
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
    this.isConnected = false;
  }

  // --- Sub-APIs ---

  public pairing = {
    request: (appName: string = 'Web Application') => {
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

  public async print(printerName: string, data: string | Uint8Array, options: PrintOptions = {}): Promise<{ jobId: string }> {
    let base64Data = '';
    if (typeof data === 'string') {
      base64Data = typeof btoa !== 'undefined' ? btoa(unescape(encodeURIComponent(data))) : '';
    } else {
      let binary = '';
      const bytes = new Uint8Array(data);
      for (let i = 0; i < bytes.byteLength; i++) {
        binary += String.fromCharCode(bytes[i]);
      }
      base64Data = typeof btoa !== 'undefined' ? btoa(binary) : '';
    }

    const payload: any = {
      printer: printerName,
      data: base64Data,
      job_name: options.jobName || 'PrintBridge SDK Print Job'
    };

    if (this.token) {
      payload.token = this.token;
    }

    const resp = await this.sendRequest('print', payload);
    return { jobId: resp.job_id };
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
      const { resolve, reject } = this.pendingRequests.get(msg.id)!;
      this.pendingRequests.delete(msg.id);
      if (msg.success) {
        resolve(msg.payload);
      } else {
        reject(new Error(msg.error || 'Request failed'));
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
        return reject(new Error('WebSocket connection not open'));
      }

      const id = `sdk_${this.reqCounter++}`;
      this.pendingRequests.set(id, { resolve, reject });

      const msg = {
        version: 1,
        id,
        type,
        payload
      };

      this.ws.send(JSON.stringify(msg));
    });
  }
}

export default PrintBridgeClient;
