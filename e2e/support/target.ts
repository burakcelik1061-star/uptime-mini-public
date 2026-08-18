/**
 * Kontrollü hedef sunucu — monitor'ün gösterdiği adres.
 *
 * TESTİN İÇİNDE AYAĞA KALKIYOR (bu adımın şartı): E2E'nin dış dünyaya hiçbir
 * bağımlılığı yok. Sağlıklıdan bozuğa geçiş bir bayrakla yapılıyor, yani
 * "kesinti" testin istediği anda ve KESİN olarak başlıyor — gerçek bir siteyi
 * beklemek, testi ağa ve şansa bağlardı.
 */
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

export class ControlledTarget {
  private server: Server;
  private healthy = true;

  private constructor(server: Server) {
    this.server = server;
  }

  /** start, efemer bir portta (0) dinlemeye başlar. */
  static async start(): Promise<ControlledTarget> {
    const server = createServer((_request, response) => {
      response.writeHead(target.healthy ? 200 : 500, {
        'content-type': 'text/plain',
      });
      response.end(target.healthy ? 'ok' : 'bozuk');
    });

    const target = new ControlledTarget(server);

    await new Promise<void>((resolve) => {
      // 127.0.0.1: worker AYNI HOST'ta yerel bir süreç olarak koşuyor
      // (bkz. support/stack.ts), yani loopback yeterli.
      server.listen(0, '127.0.0.1', resolve);
    });

    return target;
  }

  /** url, monitor'e yazılacak adres. */
  get url(): string {
    const address = this.server.address() as AddressInfo;

    return `http://127.0.0.1:${address.port}/health`;
  }

  /** breakIt, hedefi 500 döndürür hâle getirir. */
  breakIt(): void {
    this.healthy = false;
  }

  /** heal, hedefi tekrar sağlıklı yapar. */
  heal(): void {
    this.healthy = true;
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) => {
      this.server.close((error) => (error ? reject(error) : resolve()));
    });
  }
}
