import { get, post, type Me } from './api';

class Session {
  me = $state<Me | null>(null);
  ready = $state(false);

  async load() {
    try {
      this.me = await get<Me>('/api/me');
    } catch {
      this.me = null;
    } finally {
      this.ready = true;
    }
  }

  async login(name: string, password: string) {
    this.me = await post<Me>('/api/login', { name, password });
  }

  async register(code: string, name: string, password: string) {
    this.me = await post<Me>('/api/register', { code, name, password });
  }

  async logout() {
    await post('/api/logout');
    this.me = null;
  }
}

export const session = new Session();
