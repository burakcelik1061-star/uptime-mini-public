import type { ApiErrorBody, FieldError } from './api-types';

/**
 * api istemcisi — tek giriş noktası.
 *
 * Adres ortam değişkeninden geliyor ama VARSAYILANI var: depoyu klonlayan
 * biri `pnpm dev` dediğinde hiçbir .env dosyası olmadan çalışmalı
 * (bkz. docs/0.0-constraints.md). NEXT_PUBLIC_ öneki, değerin tarayıcı
 * paketine gömülmesi için gerekli.
 */
export const API_BASE_URL =
  process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:3400';

/** Sunucunun standart hata zarfının istemci tarafındaki karşılığı. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields: FieldError[];
  readonly requestId?: string;

  constructor(
    status: number,
    code: string,
    message: string,
    fields: FieldError[] = [],
    requestId?: string,
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.fields = fields;
    this.requestId = requestId;
  }

  /** Alan bazlı hataları `{ alan: mesaj }` biçiminde döner. */
  fieldMessages(): Record<string, string> {
    const messages: Record<string, string> = {};
    for (const field of this.fields) {
      if (field.field) messages[field.field] = field.message;
    }
    return messages;
  }
}

export interface RequestOptions {
  // PUT 5.1'de eklendi: status page'in servis seçimi TÜMDEN değiştiriliyor
  // (PATCH "kısmi güncelleme" demek olurdu ve gönderilen liste tam da tamamı).
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  /** Access token; yoksa Authorization başlığı hiç eklenmiyor. */
  token?: string | null;
  signal?: AbortSignal;
}

/**
 * request, api'ye tek bir istek atar ve hata zarfını ApiError'a çevirir.
 *
 * `credentials: 'include'` HER istekte: refresh cookie'sinin gönderilmesi
 * gereken uçlar (/v1/auth/*) bunu istiyor. Cookie'nin Path'i `/v1/auth`
 * olduğu için diğer uçlara zaten gönderilmiyor — yani bu ayar, cookie'yi
 * gereğinden geniş dolaştırmıyor.
 */
export async function request<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const { method = 'GET', body, token, signal } = options;

  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (token) headers.Authorization = `Bearer ${token}`;

  let response: Response;
  try {
    response = await fetch(`${API_BASE_URL}${path}`, {
      method,
      headers,
      credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (cause) {
    // Ağ hatası (api ayakta değil, CORS engeli): fetch burada `TypeError`
    // atıyor ve mesajı tarayıcıya göre değişiyor. Kullanıcıya sebebi
    // anlaşılır tek bir mesaj veriliyor.
    if (cause instanceof DOMException && cause.name === 'AbortError')
      throw cause;
    throw new ApiError(0, 'network_error', 'Sunucuya ulaşılamadı');
  }

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  const payload: unknown = text ? safeParse(text) : null;

  if (!response.ok) {
    throw toApiError(response.status, payload);
  }

  return payload as T;
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function toApiError(status: number, payload: unknown): ApiError {
  const body = payload as ApiErrorBody | null;
  const detail = body?.error;

  // Sözleşmeye uymayan bir cevap (ör. araya giren proxy'nin HTML hata
  // sayfası) sessizce "undefined" mesajına dönüşmesin.
  if (!detail) {
    return new ApiError(
      status,
      'unknown_error',
      `Beklenmeyen hata (${status})`,
    );
  }

  return new ApiError(
    status,
    detail.code,
    detail.message,
    detail.fields ?? [],
    detail.requestId,
  );
}
