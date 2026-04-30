// Auth state — single zustand store the whole app reads from.
//
// Lifecycle:
//   - On mount, AuthGate calls loadMe() once to hydrate from /auth/me.
//   - login/register/loginOAuth set state on success.
//   - logout clears + redirects to /login (caller does the navigate).
//   - switchTenant POSTs /auth/switch_tenant then re-hydrates via loadMe.
//
// Cookies do the heavy lifting — we never store JWTs in JS. The
// store only mirrors what /auth/me reports so the UI can render
// chrome (tenant switcher, user email, protected gates).

import { create } from 'zustand'
import { api, ApiError, API_BASE } from './api'

export interface User {
  id: string
  email: string
  created_at?: string
  updated_at?: string
  last_login_at?: string
  oauth_providers?: { provider: string; subject: string; email?: string; linked_at?: string }[]
}

export interface Tenant {
  id: string
  name: string
  owner_id?: string
  created_at?: string
}

export interface Membership {
  tenant: Tenant
  role: 'owner' | 'admin' | 'member' | string
}

export interface MeResponse {
  user: User
  tenant_id: string
  memberships: Membership[]
}

interface AuthState {
  loading: boolean
  // null = unauthenticated (post-load); undefined = not loaded yet
  me: MeResponse | null | undefined
  loadMe: () => Promise<void>
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  switchTenant: (tenantId: string) => Promise<void>
  // Returns the absolute URL for an OAuth start endpoint.
  oauthStartUrl: (provider: 'google' | 'github') => string
}

export const useAuthStore = create<AuthState>((set, get) => ({
  loading: false,
  me: undefined,

  async loadMe() {
    set({ loading: true })
    try {
      const me = await api.get<MeResponse>('/api/v1/auth/me')
      // eslint-disable-next-line no-console
      console.info('[auth] /me ok', { user: me.user.email, tenant: me.tenant_id })
      set({ me, loading: false })
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        // eslint-disable-next-line no-console
        console.warn('[auth] /me 401 — cookie not accepted', {
          body: err.body,
          message: err.message,
          hint: 'check Network tab: does /auth/me request carry Cookie header? if no → cookie host/path/SameSite mismatch',
        })
        set({ me: null, loading: false })
        return
      }
      set({ loading: false })
      throw err
    }
  },

  async login(email, password) {
    await api.post('/api/v1/auth/login', { email, password })
    await get().loadMe()
  },

  async register(email, password) {
    await api.post('/api/v1/auth/register', { email, password })
    await get().loadMe()
  },

  async logout() {
    try {
      await api.post('/api/v1/auth/logout')
    } finally {
      set({ me: null })
    }
  },

  async switchTenant(tenantId) {
    await api.post('/api/v1/auth/switch_tenant', { tenant_id: tenantId })
    await get().loadMe()
  },

  oauthStartUrl(provider) {
    return `${API_BASE}/auth/oauth/${provider}/start`
  },
}))
