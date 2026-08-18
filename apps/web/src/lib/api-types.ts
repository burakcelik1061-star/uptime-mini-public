/**
 * API SÖZLEŞMESİ — backend'in `internal/httpapi` katmanında dışa verdiği
 * gövdelerin TypeScript karşılığı.
 *
 * Bu dosya bilinçli olarak elle yazılıyor ve backend'in İÇ şemasından
 * (sqlc'nin ürettiği tipler) türetilmiyor — 0.2'nin kararı: frontend
 * sözleşmeye bağlanır, veritabanı şemasına değil. Şemaya eklenen bir sütun
 * (ör. 3.2'nin `next_check_at`'i) buraya kendiliğinden düşmez; sözleşme
 * genişlediğinde bu dosya bilinçli olarak güncellenir.
 */

/** monitor_status enum'u (migration 00001). */
export type MonitorStatus = 'pending' | 'up' | 'degraded' | 'down' | 'paused';

/** monitor_method enum'u (migration 00001). */
export type MonitorMethod = 'GET' | 'HEAD' | 'POST';

export interface User {
  id: string;
  email: string;
  name: string;
  createdAt: string;
}

export interface Organization {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
  /** İsteği yapan kullanıcının bu organizasyondaki rolü. */
  role: 'owner' | 'admin' | 'member';
}

export interface Monitor {
  id: string;
  name: string;
  url: string;
  method: MonitorMethod;
  checkIntervalSeconds: number;
  timeoutMs: number;
  expectedStatusMin: number;
  expectedStatusMax: number;
  /** null = "yavaş" sinyali kapalı (2.2). */
  degradedResponseMs: number | null;
  /** Kaç ardışık başarısızlıktan sonra DOWN (3.3, flapping önleme). */
  failureThreshold: number;
  /** Kaç ardışık yanıttan sonra tekrar UP (3.3). */
  recoveryThreshold: number;
  isActive: boolean;
  status: MonitorStatus;
  /** null = monitor hiç kontrol edilmedi (3.2). */
  lastCheckedAt: string | null;
  /**
   * true = ÖLÇÜM DURMUŞ (8.6). `status` son BİLİNEN durumu taşımaya devam
   * ediyor; bu bayrak "o bilgi artık güncel değil" diyor.
   *
   * KARARI SUNUCU VERİYOR: istemci `lastCheckedAt` ile kendi saatinden
   * türetseydi, aynı kural panoda ve public sayfada iki kopya olurdu ve
   * tarayıcının saati yanlışsa sonuç da yanlış olurdu.
   */
  stale: boolean;
  /** Durumun en son değiştiği an — "ne zamandır down?" (3.3). */
  statusChangedAt: string;
  createdAt: string;
  updatedAt: string;
}

/** check_outcome enum'u (migration 00001). */
export type CheckOutcome = 'success' | 'degraded' | 'failure';

/** check_error_type enum'u (migration 00001). */
export type CheckErrorType =
  | 'timeout'
  | 'dns'
  | 'connection_refused'
  | 'tls'
  | 'unexpected_status'
  | 'blocked_url'
  | 'unknown';

/** Kontrolü kimin tetiklediği (2.3). */
export type CheckTrigger = 'scheduled' | 'manual';

/**
 * Tek bir kontrol sonucu.
 *
 * Aynı şekil iki yerde: manuel kontrolün cevabı (POST …/monitors/{id}/check)
 * ve kontrol geçmişi (GET …/monitors/{id}/checks). 2.3'ten itibaren manuel
 * kontroller de saklanıyor, `triggeredBy` ile işaretli.
 */
export interface CheckResult {
  /** `bigint` — JavaScript'in Number'ı 2^53'ten sonra hassasiyet kaybettiği için METİN. */
  id: string;
  outcome: CheckOutcome;
  triggeredBy: CheckTrigger;
  /** null = cevap hiç alınamadı (bağlantı/isim/TLS hatası). */
  httpStatus: number | null;
  /** null = ölçüm yok (istek hiç tamamlanmadı). */
  responseTimeMs: number | null;
  errorType: CheckErrorType | null;
  errorMessage: string | null;
  checkedAt: string;
}

/**
 * Bir zaman aralığının ham uptime özeti (2.4).
 *
 * `uptimePercent` null olabilir: aralıkta hiç ZAMANLANMIŞ kontrol yoksa oran
 * %100 değil, BİLİNMİYOR (0.1'in bağlayıcı tanımı). Manuel kontroller
 * uptime'a girmiyor, ayrıca sayılıyor (bkz. 2.3).
 */
export interface CheckSummary {
  from: string;
  to: string;
  uptimePercent: number | null;
  scheduledChecks: number;
  successChecks: number;
  degradedChecks: number;
  failureChecks: number;
  /**
   * Planlı bakım penceresine düşen kontroller (4.3): `scheduledChecks`in
   * içinde ama uptime'ın ne payına ne paydasına giriyor — bakım kesinti
   * sayılmıyor, ama bakımdaki başarılı ölçümler de oranı yukarı çekmiyor.
   */
  maintenanceChecks: number;
  manualChecks: number;
  avgResponseTimeMs: number | null;
}

/** Kayıt, giriş ve yenileme uçlarının ortak cevabı. */
export interface AuthResponse {
  user: User;
  accessToken: string;
  /** Saniye cinsinden — istemci tazelemeyi kendi saatine göre planlamasın. */
  expiresIn: number;
}

/** Alan bazlı doğrulama hatası (422 cevaplarında). */
export interface FieldError {
  field: string;
  message: string;
}

/** Her hatanın zarfı (bkz. internal/httpapi/errors.go). */
export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    fields?: FieldError[];
    requestId?: string;
  };
}

/** Monitor oluşturma gövdesi; gönderilmeyen alanlar sunucu varsayılanı. */
export interface CreateMonitorInput {
  name: string;
  url: string;
  method?: MonitorMethod;
  checkIntervalSeconds?: number;
  timeoutMs?: number;
  expectedStatusMin?: number;
  expectedStatusMax?: number;
  degradedResponseMs?: number;
  isActive?: boolean;
}

/** incident_status enum'u (migration 00001). */
export type IncidentStatus =
  'investigating' | 'identified' | 'monitoring' | 'resolved';

/** incident_impact enum'u (migration 00001). */
export type IncidentImpact = 'none' | 'minor' | 'major' | 'critical';

/** Incident'ın etkilediği servis (4.2). Ad CANLI monitor kaydından geliyor. */
export interface AffectedMonitor {
  id: string;
  name: string;
  /** Servisin ŞU ANKİ durumu — geçmişteki değil. */
  status: MonitorStatus;
}

/**
 * Kesinti kaydı (4.1/4.2).
 *
 * `isAutoCreated` true ise incident'ı durum makinesi açtı; manuel incident'ı
 * bir admin açar ve birden fazla servisi kapsayabilir (0.1 §5).
 */
export interface Incident {
  id: string;
  title: string;
  status: IncidentStatus;
  impact: IncidentImpact;
  isAutoCreated: boolean;
  startedAt: string;
  /** null = kesinti DEVAM EDİYOR. */
  resolvedAt: string | null;
  /** null = kesinti sürüyor; "şu ana kadar" hesabı istemcinin. */
  durationSeconds: number | null;
  monitors: AffectedMonitor[];
  createdAt: string;
  updatedAt: string;
}

/** Timeline'ın tek bir satırı (4.2). */
export interface IncidentUpdate {
  id: string;
  status: IncidentStatus;
  body: string;
  /** true = satırı otomatik açma/kapama yazdı (4.1). */
  isSystem: boolean;
  /** null = sistem satırı ya da kullanıcısı silinmiş satır. */
  authorId: string | null;
  createdAt: string;
}

/** Planlı bakım penceresi (4.3). */
export interface MaintenanceWindow {
  id: string;
  title: string;
  description: string | null;
  startsAt: string;
  endsAt: string;
  monitors: AffectedMonitor[];
  createdAt: string;
}

/* --- Public status page (5.1) ---------------------------------------------
 *
 * Bu bölümdeki tipler OTURUMSUZ ucun (`/v1/public/status/{slug}`) sözleşmesi ve
 * admin tiplerinden bilinçli olarak AYRI: public gövde daha dar (izlenen URL,
 * yazar kimliği, `isAutoCreated` yok). Aynı tipi paylaşsalardı, admin
 * sözleşmesine eklenen bir alan public sayfada da varmış gibi görünürdü.
 */

/** Sayfanın tek satırlık genel durumu — KARARI sunucunun (statuspage.Summarize). */
export type OverallStatus =
  | 'operational'
  | 'degraded'
  | 'partial_outage'
  | 'major_outage'
  | 'maintenance'
  | 'unknown';

/**
 * 90 günlük grafikteki tek bir günün hâli (5.2).
 *
 * `no_data` ile `up` ayrımı bu sözleşmenin en önemli parçası: "veri yok" günü
 * yeşil görünemez (0.1'in bağlayıcı kuralı, 0.2'nin açık şartı).
 */
export type UptimeDayState =
  'no_data' | 'maintenance' | 'up' | 'partial' | 'down';

/** 90 günlük grafikteki tek bir bar (5.2). Veri 3.4'ün günlük özetlerinden. */
export interface UptimeDay {
  /** UTC takvim günü (YYYY-AA-GG) — zaman damgası değil. */
  date: string;
  state: UptimeDayState;
  /** null = oran YOK (veri yok ya da gün tamamen bakımda). %100 ile karıştırma. */
  uptimePercent: number | null;
  scheduledChecks: number;
  maintenanceChecks: number;
  /** O güne düşen kesintiler — bar üstünde gösteriliyor. */
  incidents: UptimeDayIncident[];
}

export interface UptimeDayIncident {
  id: string;
  title: string;
  impact: IncidentImpact;
  /** true = kesinti hâlâ sürüyor. */
  isOpen: boolean;
}

/** Yayımlanan bir servis. İzlenen URL public sözleşmede YOK. */
export interface PublicService {
  id: string;
  name: string;
  status: MonitorStatus;
  /** Durumun en son değiştiği an — "ne zamandır böyle". */
  statusChangedAt: string;
  /** true = servis ŞU AN planlı bakımda (4.3). */
  inMaintenance: boolean;
  /** true = bu servisin ÖLÇÜMÜ DURMUŞ (8.6). */
  stale: boolean;
  /**
   * Ölçümün durduğu an — YALNIZCA `stale` true iken dolu.
   *
   * 5.1 `lastCheckedAt`i public sözleşmeden bilinçli olarak çıkarmıştı
   * (kontrol sıklığı iç bilgi); bayat bir servis tanımı gereği kendi
   * aralığında kontrol edilmediği için bu alan o bilgiyi taşımıyor.
   */
  staleSince: string | null;
  /** Son 90 gün — HER ZAMAN tam 90 eleman, verisi olmayan günler dahil (5.2). */
  history: UptimeDay[];
  /** Servisin grubu; null = gruplanmamış (5.3). */
  group: string | null;
}

/** Timeline satırı — YAZARSIZ (org üyelerinin kimlikleri public değil). */
export interface PublicIncidentUpdate {
  id: string;
  status: IncidentStatus;
  body: string;
  isSystem: boolean;
  createdAt: string;
}

/** Etkilenen servis — yalnızca YAYIMLANANLAR listeleniyor. */
export interface PublicAffectedService {
  id: string;
  name: string;
  status: MonitorStatus;
}

export interface PublicIncident {
  id: string;
  title: string;
  status: IncidentStatus;
  impact: IncidentImpact;
  startedAt: string;
  /** null = kesinti DEVAM EDİYOR. */
  resolvedAt: string | null;
  durationSeconds: number | null;
  services: PublicAffectedService[];
  updates: PublicIncidentUpdate[];
}

/** Aktif veya yaklaşan bakım penceresi (geçmiş olanlar dönmüyor). */
export interface PublicMaintenanceWindow {
  id: string;
  title: string;
  description: string | null;
  startsAt: string;
  endsAt: string;
  services: PublicAffectedService[];
}

/**
 * Tema renginin ÜZERİNE yazılacak metnin tonu (5.3).
 *
 * Sunucu RENK değil TON gönderiyor: ham bir metin rengi göndermek, arayüzde
 * ham değer yasağını (0.2) sunucu üzerinden delmek olurdu.
 */
export type ThemeTone = 'light' | 'dark';

/** `GET /v1/public/status/{slug}` cevabının tamamı. */
export interface PublicStatusPage {
  slug: string;
  /** ÇÖZÜLMÜŞ başlık: org yazmadıysa organizasyonun adı (5.3). */
  title: string;
  /** null = açıklama yok. */
  description: string | null;
  /** null = logo yok. https ile doğrulanmış dış adres. */
  logoUrl: string | null;
  /** null = varsayılan vurgu rengi. `#rrggbb`. */
  themeColor: string | null;
  themeTone: ThemeTone;
  /** Servis gruplarının gösterim sırası (5.3). */
  groups: string[];
  overall: OverallStatus;
  /** Ölçümü durmuş yayımlanmış servis sayısı — sayfa üstü notu besliyor (8.6). */
  staleServices: number;
  services: PublicService[];
  /** Açık ve son kapanmış kesintiler, yeniden eskiye. */
  incidents: PublicIncident[];
  maintenance: PublicMaintenanceWindow[];
}

/** Org'un status page ayarları (admin tarafı, 5.1 + 5.3). */
export interface StatusPageSettings {
  slug: string;
  isPublished: boolean;
  /** null = organizasyonun adı kullanılıyor. */
  title: string | null;
  description: string | null;
  logoUrl: string | null;
  /** null = varsayılan vurgu rengi. */
  themeColor: string | null;
  /** Path tabanlı public adres — öneki sunucu kuruyor (5.4 değiştirebilir). */
  publicPath: string;
  createdAt: string;
  updatedAt: string;
}

/** Yayımlanan tek bir servis (admin tarafı: URL de dönüyor). */
export interface StatusPageItem {
  monitorId: string;
  name: string;
  url: string;
  status: MonitorStatus;
  /** null = gruplanmamış (5.3). */
  group: string | null;
}

/** `GET/PATCH/PUT …/status-page…` cevaplarının ortak gövdesi. */
export interface StatusPageResponse {
  statusPage: StatusPageSettings;
  /** Grupların gösterim SIRASI. */
  groups: string[];
  items: StatusPageItem[];
}

/**
 * Servis seçimi + gruplamanın TAMAMI (`PUT …/status-page/services`).
 *
 * İkisi tek gövdede: ayrı yazılsalardı "servisleri olmayan grup" ve "silinmiş
 * gruba işaret eden servis" diye iki ara durum doğardı.
 */
export interface ReplaceStatusPageServicesInput {
  groups: string[];
  items: { monitorId: string; group: string | null }[];
}

/**
 * Sayfa ayarlarının kısmi güncellemesi (`PATCH …/status-page`).
 *
 * Alanlar ÜÇ DURUMLU: gönderilmedi (dokunma) · null (temizle) · değer.
 * `undefined` ile `null` bu yüzden ayrı: `JSON.stringify` gönderilmeyen alanı
 * atlıyor, `null`ı ise yazıyor.
 */
export interface UpdateStatusPageInput {
  isPublished?: boolean;
  title?: string | null;
  description?: string | null;
  logoUrl?: string | null;
  themeColor?: string | null;
}

/** Manuel incident oluşturma gövdesi (4.4). */
export interface CreateIncidentInput {
  title: string;
  impact: IncidentImpact;
  monitorIds: string[];
  /** Timeline'ın ilk satırı: "ne oluyor" cümlesi kayıtla birlikte doğuyor. */
  body: string;
}

/** Timeline'a güncelleme ekleme gövdesi (4.2). */
export interface CreateIncidentUpdateInput {
  status: IncidentStatus;
  body: string;
}

/** Bakım penceresi oluşturma gövdesi (4.3). */
export interface CreateMaintenanceWindowInput {
  title: string;
  description?: string;
  /** RFC 3339 — sunucu geçmişe dönük pencereyi reddediyor. */
  startsAt: string;
  endsAt: string;
  monitorIds: string[];
}

/** Bildirim kanalı (6.3). */
export interface NotificationChannel {
  id: string;
  kind: 'email' | 'webhook';
  name: string;
  target: string;
  /** Gönderim özeti: "kanal çalışıyor mu" sorusunun cevabı. */
  deliveries: {
    sent: number;
    pending: number;
    failed: number;
  };
  createdAt: string;
}

export interface CreateNotificationChannelInput {
  kind: 'email' | 'webhook';
  name: string;
  target: string;
}

/** Organizasyon üyesi (1.4'ün `GET …/members` ucu). */
export interface OrganizationMember {
  userId: string;
  email: string;
  name: string;
  role: OrgRole;
  joinedAt: string;
}

/** org_role enum'u (migration 00001). */
export type OrgRole = 'owner' | 'admin' | 'member';

/** org_plan enum'u (migration 00021). */
export type OrgPlan = 'free' | 'pro';

/**
 * Bir planın sınırları (8.5).
 *
 * SAYILAR SUNUCUDAN GELİYOR, istemcide sabit DEĞİL: 2.1'in "doğrulama
 * kuralları istemcide tekrarlanmaz" kararının kota tarafındaki karşılığı.
 * İki yere kopyalanan bir limit, altı ay sonra sessizce ayrışırdı.
 */
export interface PlanLimits {
  maxMonitors: number;
  minCheckIntervalSeconds: number;
}

export interface PlanOption {
  plan: OrgPlan;
  label: string;
  limits: PlanLimits;
}

export interface PlanState {
  plan: OrgPlan;
  label: string;
  limits: PlanLimits;
  usage: { monitors: number };
  /** Seçilebilir planlar — karşılaştırma ekranda sunucudan besleniyor. */
  available: PlanOption[];
}

/**
 * Bekleyen davet (8.3).
 *
 * TOKEN YOK: linki yeniden göstermek, daveti iptal edip yenisini
 * göndermekten daha zayıf bir güvenlik hikâyesi olurdu (sunucu da
 * döndürmüyor).
 */
export interface Invitation {
  id: string;
  email: string;
  role: OrgRole;
  expiresAt: string;
  createdAt: string;
  /** null = davet eden kullanıcı silinmiş (migration 00019 → SET NULL). */
  invitedByEmail: string | null;
}

export interface CreateInvitationInput {
  email: string;
  role: OrgRole;
}

/**
 * `POST /v1/invitations/accept` cevabı (8.3).
 *
 * `Organization`DAN AYRI ve DAHA DAR: kabul akışı organizasyonun yalnızca
 * kimliğini, adını ve verilen rolü biliyor. Ortak tipi kullanmak, sunucunun
 * `slug: ""` gibi sıfır değerler göndermesi demekti — istemci onları gerçek
 * sanardı. Tam kayıt panoya gidince `GET /v1/orgs` ile okunuyor.
 */
export interface AcceptedOrganization {
  id: string;
  name: string;
  role: OrgRole;
}

/**
 * Geliştirici posta kutusundaki tek bir mesaj (8.2).
 *
 * SÖZLEŞMENİN GERİ KALANINDAN FARKI: bu bir ürün yüzeyi değil, MOCK e-posta
 * adapter'ının posta kutusu. Ucu (`GET /v1/dev/outbox`) yalnızca APP_ENV
 * açıkça development|test iken sunucuda kayıtlı; aksi hâlde 404 dönüyor ve
 * arayüz bunu "kapalı" olarak gösteriyor.
 */
export interface OutboxMessage {
  id: string;
  recipient: string;
  subject: string;
  /** Düz metin. Ekran içindeki http(s) linklerini tıklanabilir yapıyor. */
  body: string;
  createdAt: string;
}
