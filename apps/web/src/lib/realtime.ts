'use client';

import { useEffect, useRef, useState } from 'react';

import { API_BASE_URL } from './api';
import { useSession } from './session';

/**
 * Canlı yayının istemci tarafı (Faz 6.1).
 *
 * KİMLİK NEDEN BİLETLE TAŞINIYOR: tarayıcının `WebSocket` API'si isteğe
 * başlık ekleyemiyor (fetch'in aksine ikinci parametresi yalnızca
 * alt-protokol listesi). Access token 2.1'den beri yalnızca bellekte ve
 * refresh cookie'sinin Path'i `/v1/auth` — yani el sıkışmaya kendiliğinden
 * hiçbir kimlik bilgisi gitmiyor. İstemci bu yüzden önce NORMAL bir istekle
 * (Authorization başlığıyla) 30 saniyelik, TEK KULLANIMLIK bir bilet alıyor.
 *
 * YENİDEN BAĞLANMA BURADA, kütüphanede değil: tarayıcının WebSocket'i
 * kopunca kendiliğinden bağlanmıyor ve her yeni bağlantı YENİ BİR BİLET
 * gerektiriyor (eskisi yakıldı), yani "aynı adrese tekrar bağlan" yetmiyor.
 */

/** Olayın istemci tarafındaki karşılığı (sunucudaki realtime.Event). */
export interface RealtimeEvent {
  type: string;
  at: string;
  data?: unknown;
}

export type RealtimeStatus = 'idle' | 'connecting' | 'live' | 'offline';

/** Sunucunun bağlantı kurulduğunda gönderdiği ilk olay. */
const EVENT_READY = 'realtime.ready';

/**
 * OLAY OTOBÜSÜ (6.2).
 *
 * Bağlantıyı TEK BİR bileşen açıyor (üst çubuk: `AppHeader`), olayları
 * kullanan ekranlar ise `useRealtimeEvent` ile buraya abone oluyor. Neden
 * modül seviyesinde bir küme, React context değil: context, olayı tüketen her
 * ekranın bir sağlayıcıyla sarılmasını gerektirirdi ve pano sayfaları
 * organizasyonu kendileri okuduğu için sağlayıcının doğal bir yeri yok. Soket
 * zaten sekme başına tek bir kaynak — modül durumu onun tam karşılığı.
 *
 * KURAL: `useRealtime` yalnızca `AppHeader` içinde çağrılır. İkinci bir
 * çağıran ikinci bir soket (ve ikinci bir bilet) demek olurdu.
 */
type EventHandler = (event: RealtimeEvent) => void;

const handlers = new Set<EventHandler>();

function emit(event: RealtimeEvent) {
  for (const handler of handlers) handler(event);
}

const MIN_RETRY_MS = 1_000;
const MAX_RETRY_MS = 30_000;

/**
 * Bekleme süresi: üssel artış + %20 sapma.
 *
 * Sapma şart: api yeniden başladığında bağlı olan HER sekme aynı anda kopuyor.
 * Sabit gecikmeyle hepsi aynı milisaniyede geri gelir ve yeni açılan sunucuyu
 * ilk işi olarak bir bilet dalgasıyla karşılardı.
 */
function retryDelay(attempt: number): number {
  const base = Math.min(MIN_RETRY_MS * 2 ** (attempt - 1), MAX_RETRY_MS);
  return Math.round(base * (0.8 + Math.random() * 0.4));
}

function socketURL(ticket: string): string {
  // http → ws, https → wss.
  const base = API_BASE_URL.replace(/^http/, 'ws');
  return `${base}/v1/ws?ticket=${encodeURIComponent(ticket)}`;
}

/**
 * useRealtime, verilen organizasyonun canlı kanalına bağlanır.
 *
 * `orgId` null olduğunda (veya oturum yokken) hiç bağlanmıyor: oturumsuz bir
 * sekmenin bilet istemesi her açılışta kesin başarısız olacak bir istek
 * demekti.
 */
export function useRealtime(orgId: string | null): RealtimeStatus {
  const { authFetch, status: sessionStatus } = useSession();
  const [status, setStatus] = useState<RealtimeStatus>('connecting');

  // "Bağlanmalı mıyız" TÜRETİLMİŞ bir değer, state değil: kapalı hâli ayrıca
  // yazmak, bağlantının açık olup olmadığı sorusuna iki ayrı cevap doğururdu.
  const enabled = orgId !== null && sessionStatus === 'authenticated';

  useEffect(() => {
    if (!enabled || !orgId) return;

    // `closed`, effect temizlendikten sonra iş yapılmasını engelliyor:
    // devam eden bilet isteği, zamanlanmış yeniden deneme ve açılmakta olan
    // soket bu bayrağa bakıyor.
    let closed = false;
    let socket: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;

    const scheduleRetry = () => {
      if (closed) return;
      attempt += 1;
      timer = setTimeout(() => void connect(), retryDelay(attempt));
    };

    const connect = async () => {
      if (closed) return;
      setStatus(attempt === 0 ? 'connecting' : 'offline');

      let ticket: string;
      try {
        ({ ticket } = await authFetch<{ ticket: string; expiresIn: number }>(
          `/v1/orgs/${orgId}/realtime-ticket`,
          { method: 'POST' },
        ));
      } catch {
        // Bilet alınamadı (api kapalı, oturum düştü): sebep ne olursa olsun
        // yapılacak şey aynı — bekle ve yeniden dene.
        if (!closed) {
          setStatus('offline');
          scheduleRetry();
        }
        return;
      }

      if (closed) return;

      const next = new WebSocket(socketURL(ticket));
      socket = next;

      next.onmessage = (message: MessageEvent<string>) => {
        let event: RealtimeEvent;
        try {
          event = JSON.parse(message.data) as RealtimeEvent;
        } catch {
          return;
        }

        if (event.type === EVENT_READY) {
          // DURUM `onopen`'DA DEĞİL BURADA "canlı" oluyor: soketin açılması
          // yalnızca el sıkışmanın tamamlandığını söyler. Sunucunun abone
          // listesine gerçekten girdiğimizi söyleyen tek şey bu olay. Yeniden
          // deneme sayacı da bu yüzden burada sıfırlanıyor — açılıp hemen
          // kopan bir bağlantı beklemeyi kısaltmamalı.
          attempt = 0;
          setStatus('live');
        }

        // `realtime.ready` DE OTOBÜSE GİRİYOR ve bu 6.2'nin "kopukken
        // kaçırılan olay" kararının tamamı: olaylar gövdesiz olduğu için
        // istemcinin her olaya verdiği cevap "tazele". Yeniden bağlanmak da
        // bir olaydır — kaçırılanları oynatmaya, olay tamponuna veya "since"
        // imlecine gerek kalmıyor, bağlantı geri geldiği anda ekran güncel.
        emit(event);
      };

      next.onclose = () => {
        if (closed || socket !== next) return;
        setStatus('offline');
        scheduleRetry();
      };

      // `onerror` ayrıca ele alınmıyor: tarayıcı hatadan sonra HER ZAMAN
      // `close` da tetikliyor, yani yeniden deneme oradan zaten kuruluyor.
      // İkisini de bağlamak aynı kopma için iki deneme zamanlardı.
    };

    void connect();

    return () => {
      closed = true;
      clearTimeout(timer);
      // 1000 (normal): sunucu bunu kasıtlı bir ayrılış olarak görüyor ve
      // aboneliği düşürüyor.
      socket?.close(1000, '');
      socket = null;
    };
  }, [enabled, orgId, authFetch]);

  return enabled ? status : 'idle';
}

/**
 * useRealtimeEvent, canlı olaylara abone olur.
 *
 * Handler ref'te tutuluyor: her render'da yeniden üretilen bir fonksiyon
 * bağımlılık listesine girseydi, abonelik her render'da sökülüp yeniden
 * kurulurdu.
 */
export function useRealtimeEvent(handler: EventHandler): void {
  const handlerRef = useRef(handler);

  useEffect(() => {
    handlerRef.current = handler;
  }, [handler]);

  useEffect(() => {
    const wrapped: EventHandler = (event) => handlerRef.current(event);
    handlers.add(wrapped);

    return () => {
      handlers.delete(wrapped);
    };
  }, []);
}

/** Tazeleme çağrılarının birleştirilme penceresi. */
const REFRESH_DEBOUNCE_MS = 300;

/**
 * useRealtimeRefresh, her canlı olayda verilen tazelemeyi çalıştırır —
 * ama BİRLEŞTİREREK.
 *
 * Neden gerekli: bir ağ kesintisi aynı anda onlarca monitor'ü düşürebiliyor ve
 * her biri ayrı bir olay üretiyor. Birleştirme olmasaydı tek bir olay dalgası
 * onlarca eşzamanlı istek demekti — ölçüm aracının kendi paniğiyle kendi
 * API'sini yorması.
 */
export function useRealtimeRefresh(refresh: () => void): void {
  const refreshRef = useRef(refresh);

  useEffect(() => {
    refreshRef.current = refresh;
  }, [refresh]);

  const timerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useRealtimeEvent(() => {
    clearTimeout(timerRef.current);
    timerRef.current = setTimeout(
      () => refreshRef.current(),
      REFRESH_DEBOUNCE_MS,
    );
  });

  useEffect(() => () => clearTimeout(timerRef.current), []);
}
