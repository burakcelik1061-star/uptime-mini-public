'use client';

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';

import { ApiError, request, type RequestOptions } from './api';
import type { AuthResponse, User } from './api-types';

/**
 * Oturumun istemci tarafı (Faz 2.1 kararı).
 *
 * ACCESS TOKEN YALNIZCA BELLEKTE: localStorage/sessionStorage KULLANILMIYOR.
 * Oraya yazılan bir token'ı XSS ile çalışan herhangi bir script okuyabilir ve
 * saldırgan onu dışarı taşıyabilir. Bellekteki değer sekme kapanınca gidiyor;
 * oturumun devamlılığını sağlayan şey httpOnly refresh cookie'si (1.2) —
 * sayfa yenilendiğinde token /v1/auth/refresh ile yeniden alınıyor.
 */

type SessionStatus = 'loading' | 'anonymous' | 'authenticated';

interface SessionValue {
  status: SessionStatus;
  user: User | null;
  login: (email: string, password: string) => Promise<void>;
  /**
   * Yeni hesap açar ve oturumu AÇIK bırakır (8.1).
   *
   * Ayrı bir `login` çağrısı yok: `POST /v1/auth/register` 1.1'den beri
   * giriş ucuyla AYNI gövdeyi (`AuthResponse`) döndürüyor ve refresh
   * cookie'sini de aynı yerden (`respondWithSession`) yazıyor. Kayıttan
   * sonra bir de giriş yapmak, aynı kullanıcı için ikinci bir oturum ailesi
   * açmak ve 1.2'nin rotasyonunu boşuna bir tur çalıştırmak olurdu.
   */
  register: (email: string, password: string, name: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Kimlik doğrulamalı istek; 401'de bir kez tazeleyip yeniden dener. */
  authFetch: <T>(path: string, options?: RequestOptions) => Promise<T>;
}

const SessionContext = createContext<SessionValue | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<SessionStatus>('loading');
  const [user, setUser] = useState<User | null>(null);

  // Token state DEĞİL ref: her istekte GÜNCEL değeri okumak gerekiyor.
  // State olsaydı, tazelemeden hemen sonra çalışan bir istek kapanışta
  // kalmış eski token'ı kullanabilirdi.
  const tokenRef = useRef<string | null>(null);

  // Devam eden tazeleme isteği. TEK UÇUŞ (single-flight) ŞART: üç istek
  // birden 401 alırsa üç ayrı /v1/auth/refresh çağrısı doğardı ve sunucu
  // ikinci çağrıyı "kullanılmış token yeniden sunuldu" sayıp OTURUM AİLESİNİ
  // TAMAMEN İPTAL ederdi (bkz. docs/1.2-refresh-tokens.md). Yani burada
  // yapılan basit bir optimizasyon değil, sözleşmenin gereği.
  const refreshRef = useRef<Promise<string | null> | null>(null);

  const applySession = useCallback((response: AuthResponse) => {
    tokenRef.current = response.accessToken;
    setUser(response.user);
    setStatus('authenticated');
  }, []);

  const clearSession = useCallback(() => {
    tokenRef.current = null;
    setUser(null);
    setStatus('anonymous');
  }, []);

  const refresh = useCallback((): Promise<string | null> => {
    refreshRef.current ??= request<AuthResponse>('/v1/auth/refresh', {
      method: 'POST',
    })
      .then((response) => {
        applySession(response);
        return response.accessToken;
      })
      .catch(() => {
        // Cookie yok, süresi dolmuş veya iptal edilmiş — hepsi aynı sonuç:
        // kullanıcı anonim (sunucu da bu üçünü ayırt etmiyor).
        clearSession();
        return null;
      })
      .finally(() => {
        refreshRef.current = null;
      });

    return refreshRef.current;
  }, [applySession, clearSession]);

  // Sayfa ilk açıldığında oturumu KURTARMA denemesi: refresh cookie'si varsa
  // kullanıcı giriş ekranını hiç görmüyor.
  useEffect(() => {
    void refresh();
  }, [refresh]);

  const authFetch = useCallback(
    async <T,>(path: string, options: RequestOptions = {}): Promise<T> => {
      try {
        return await request<T>(path, { ...options, token: tokenRef.current });
      } catch (error) {
        if (!(error instanceof ApiError) || error.status !== 401) throw error;

        // Access token'ın ömrü 15 dakika; sekme açık kalınca ilk 401 burada
        // karşılanıyor ve kullanıcı hiçbir şey fark etmiyor.
        const token = await refresh();
        if (!token) throw error;

        // Yalnızca BİR kez yeniden deneniyor: taze token'la gelen ikinci bir
        // 401 gerçekten yetkisizlik demek, döngüye girmenin anlamı yok.
        return await request<T>(path, { ...options, token });
      }
    },
    [refresh],
  );

  const login = useCallback(
    async (email: string, password: string) => {
      const response = await request<AuthResponse>('/v1/auth/login', {
        method: 'POST',
        body: { email, password },
      });
      applySession(response);
    },
    [applySession],
  );

  const register = useCallback(
    async (email: string, password: string, name: string) => {
      const response = await request<AuthResponse>('/v1/auth/register', {
        method: 'POST',
        body: { email, password, name },
      });
      applySession(response);
    },
    [applySession],
  );

  const logout = useCallback(async () => {
    try {
      // Sunucu oturum ailesini iptal ediyor ve cookie'yi siliyor; cevap her
      // durumda 204.
      await request<void>('/v1/auth/logout', { method: 'POST' });
    } finally {
      // Sunucuya ulaşılamasa bile yerel oturum kapanmalı.
      clearSession();
    }
  }, [clearSession]);

  const value = useMemo<SessionValue>(
    () => ({ status, user, login, register, logout, authFetch }),
    [status, user, login, register, logout, authFetch],
  );

  return (
    <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
  );
}

export function useSession(): SessionValue {
  const value = useContext(SessionContext);
  if (!value) {
    throw new Error('useSession, SessionProvider içinde kullanılmalı');
  }

  return value;
}
