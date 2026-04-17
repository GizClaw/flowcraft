import { create } from 'zustand';

interface AuthState {
  authenticated: boolean;
  accountSetup: boolean;
  loading: boolean;
  setAuthenticated: (authenticated: boolean) => void;
  setAccountSetup: (setup: boolean) => void;
  setLoading: (loading: boolean) => void;
}

export const useAuthStore = create<AuthState>((set) => ({
  authenticated: false,
  accountSetup: false,
  loading: true,
  setAuthenticated: (authenticated) => set({ authenticated }),
  setAccountSetup: (accountSetup) => set({ accountSetup }),
  setLoading: (loading) => set({ loading }),
}));
