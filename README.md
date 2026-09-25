# storage

API de objetos compatible con S3. No usa la cuenta ni los servidores de AWS: el binario guarda los archivos en disco y habla el mismo protocolo, así que sirven el SDK de AWS, la CLI y Easypanel. No hay buckets públicos.

## Arranque

```bash
go build -o bin/storage ./cmd/storage
./bin/storage
```

La primera vez crea `data/credentials.json` (permisos `0600`) e imprime el access key y el secret. El servidor escucha en `127.0.0.1:9000`.

Variables: `STORAGE_ADDR`, `PORT`, `STORAGE_DATA`, `STORAGE_REGION`, `STORAGE_ACCESS_KEY`, `STORAGE_SECRET_KEY`, `STORAGE_PUBLIC_URL`, `STORAGE_TLS_CERT`, `STORAGE_TLS_KEY`.

## Docker y Easypanel

```bash
docker compose up --build
```

En Easypanel crea un servicio desde este repositorio (tiene `Dockerfile`), publica el puerto `9000` y monta un volumen en `/data`. En el panel define:

| Variable | Ejemplo |
|---|---|
| `STORAGE_ACCESS_KEY` | una clave propia |
| `STORAGE_SECRET_KEY` | el secreto |
| `STORAGE_PUBLIC_URL` | `https://storage.tudominio.com` |

Easypanel inyecta `PORT`. Si esa variable existe, el proceso escucha en `0.0.0.0:$PORT`. El panel termina TLS; el contenedor habla HTTP por dentro. El cliente apunta `endpoint` a la URL pública del servicio, con `forcePathStyle: true`.

## Cliente

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:9000

aws s3 mb s3://photos
aws s3 cp ./foto.jpg s3://photos/foto.jpg
aws s3 presign s3://photos/foto.jpg
```

Con el SDK de AWS (el mismo patrón que Railway) usa path-style en local:

```js
import { S3Client } from "@aws-sdk/client-s3";

const s3 = new S3Client({
  region: "us-east-1",
  endpoint: "http://127.0.0.1:9000",
  forcePathStyle: true,
  credentials: {
    accessKeyId: process.env.STORAGE_ACCESS_KEY,
    secretAccessKey: process.env.STORAGE_SECRET_KEY,
  },
});
```

También acepta el estilo virtual-hosted de S3: con endpoint `http://localhost:9000` el cliente puede llamar a `http://photos.localhost:9000/foto.jpg`.

## Qué implementa

- Buckets: crear, borrar, listar, head, location
- Objetos: put, get, head, delete, copy, tags
- ListObjects y ListObjectsV2, con prefix, delimiter y paginación
- Borrado múltiple, multipart upload y URLs prefirmadas (hasta 90 días)
- CORS de navegador (abierto en local hasta que configures `PutBucketCors`)
- Cuerpo `aws-chunked` que envían los SDK actuales, con checksum CRC32, CRC32C, SHA1 y SHA256
- Rangos HTTP en las descargas

Los objetos quedan en `data/b/<bucket>/`. Una escritura se publica con rename solo cuando el cuerpo y el checksum cierran bien.
