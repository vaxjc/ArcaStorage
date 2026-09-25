# Arca

Arca es un cofre para archivos. Un solo binario guarda objetos en disco y habla la API S3, el idioma que ya usan los SDK, la CLI y paneles como Easypanel.

Amazon no participa. Arca es el servidor. Los bytes viven en tu máquina o en el volumen que le montes. Un CRM, una app o un script se conectan a tu URL con tus claves.

Sirve para adjuntos, contratos, fotos, exportaciones y cualquier archivo que no quieras meter en la base de datos. Cada bucket es privado: sin firma, o sin una URL prefirmada vigente, no se lee nada.

## Qué incluye

- Buckets: crear, listar, consultar y borrar
- Objetos: subir, bajar, consultar, copiar, borrar y etiquetar
- Listados con prefijo, delimitador y paginación
- Borrado de varios objetos a la vez
- Subidas multipart para archivos grandes
- URLs prefirmadas, hasta 90 días
- CORS para que el navegador suba directo
- Cuerpo `aws-chunked` y checksums CRC32, CRC32C, SHA-1 y SHA-256
- Descargas con rangos
- Un binario estático, sin base de datos y sin servicios externos

## Qué no hace

Arca no replica entre servidores, no cifra el disco, no tiene panel web y no administra usuarios. Hay una sola pareja de claves para todo el servidor. El permiso fino (este usuario ve esta carpeta) lo pone tu aplicación, igual que con cualquier bucket privado.

## Seguridad

Arca está pensado para publicarse y para desplegarse detrás de HTTPS.

- Toda operación exige AWS Signature V4, el esquema de firma de la API S3. La firma demuestra que el cliente posee el secret. El secret no viaja en la petición.
- Las URLs prefirmadas caducan. El navegador sube o baja un archivo sin ver la clave.
- Un objeto se publica con un rename solo cuando el cuerpo y el checksum terminan bien.
- Las claves con `..` se rechazan.
- En local escucha en `127.0.0.1`. En Docker y en Easypanel escucha en el puerto que indique `PORT`.
- Las credenciales generadas se guardan en `credentials.json` con permisos `0600`.

Antes de exponerlo a internet:

1. Pon `ARCA_PUBLIC_URL` con la URL `https` real.
2. Define `ARCA_ACCESS_KEY` y `ARCA_SECRET_KEY` largas, o usa las que Arca genera en el primer arranque y no las dejes en el repositorio.
3. Monta un volumen persistente en `/data` y haz copias de ese volumen.
4. Si el navegador sube directo, restringe el CORS del bucket a tu dominio.
5. No subas `data/` ni `credentials.json`. Ya están en `.gitignore`.

El disco guarda los archivos tal cual. Si el volumen cae en manos de alguien, los bytes se leen. Cifra el disco del servidor si ese riesgo te importa.

La licencia es MIT. Publicar este código es seguro: no trae secretos. Operarlo en público exige las claves y el HTTPS de arriba. No ha pasado una auditoría externa.

## Arranque local

```bash
go build -o bin/arca ./cmd/arca
./bin/arca
```

La primera vez crea `data/credentials.json` e imprime el access key y el secret. El servidor queda en `http://127.0.0.1:9000`.

## Docker

```bash
docker compose up --build
```

El puerto es `9000`. Los archivos quedan en el volumen `arca-data`. La primera vez las credenciales salen en el log del contenedor y se guardan dentro del volumen.

## Easypanel

Crea un servicio desde este repositorio. El `Dockerfile` ya está en la raíz. Publica el puerto `9000` y monta un volumen en `/data`.

| Variable | Ejemplo |
|---|---|
| `ARCA_ACCESS_KEY` | una clave larga |
| `ARCA_SECRET_KEY` | un secreto largo |
| `ARCA_PUBLIC_URL` | `https://arca.tudominio.com` |
| `ARCA_REGION` | `us-east-1` |

Easypanel inyecta `PORT` y termina TLS. El contenedor habla HTTP por dentro. También se aceptan los nombres viejos `STORAGE_*` si ya los tenías definidos.

## Conectar un CRM

En la base del CRM guardas la clave del objeto, por ejemplo `clientes/42/contrato.pdf`. El archivo no pasa por la base.

El backend de Arca no se llama desde el navegador con el secret. El CRM crea una URL prefirmada y el navegador hace `PUT` o `GET` contra esa URL.

```js
import { S3Client, PutObjectCommand, GetObjectCommand, DeleteObjectCommand } from "@aws-sdk/client-s3";
import { getSignedUrl } from "@aws-sdk/s3-request-presigner";

const arca = new S3Client({
  region: "us-east-1",
  endpoint: process.env.ARCA_PUBLIC_URL,
  forcePathStyle: true,
  credentials: {
    accessKeyId: process.env.ARCA_ACCESS_KEY,
    secretAccessKey: process.env.ARCA_SECRET_KEY,
  },
});

const bucket = "crm";

export function uploadUrl(key, contentType) {
  return getSignedUrl(
    arca,
    new PutObjectCommand({ Bucket: bucket, Key: key, ContentType: contentType }),
    { expiresIn: 300 }
  );
}

export function downloadUrl(key) {
  return getSignedUrl(
    arca,
    new GetObjectCommand({ Bucket: bucket, Key: key }),
    { expiresIn: 300 }
  );
}

export function removeObject(key) {
  return arca.send(new DeleteObjectCommand({ Bucket: bucket, Key: key }));
}
```

El paquete `@aws-sdk/client-s3` solo habla el protocolo. `endpoint` es tu Arca.

El bucket se crea una vez:

```bash
aws s3 mb s3://crm --endpoint-url "$ARCA_PUBLIC_URL"
```

Flujo típico:

1. El usuario elige un archivo en el CRM.
2. El backend devuelve `uploadUrl("clientes/42/contrato.pdf", "application/pdf")`.
3. El navegador envía el archivo con `PUT` a esa URL.
4. El CRM guarda la clave junto al cliente.
5. Al abrir la ficha, el backend devuelve `downloadUrl` y redirige, o descarga el objeto y lo entrega él.
6. Al borrar el adjunto, llama a `removeObject` con la misma clave.

## Configuración

| Variable | Default | Uso |
|---|---|---|
| `ARCA_ADDR` | `127.0.0.1:9000` | Dirección de escucha. En contenedor manda `PORT` si `ARCA_ADDR` está vacía |
| `PORT` | | Puerto en todas las interfaces. Lo pone Easypanel |
| `ARCA_DATA` | `data` | Directorio de los objetos. En la imagen es `/data` |
| `ARCA_REGION` | `us-east-1` | Región que Arca informa a los clientes |
| `ARCA_ACCESS_KEY` | se genera | Access key |
| `ARCA_SECRET_KEY` | se genera | Secret. No lo imprimas en logs de producción |
| `ARCA_PUBLIC_URL` | `http://localhost:9000` | URL pública, con `https` cuando hay dominio |
| `ARCA_TLS_CERT` | | Certificado, si Arca termina TLS él mismo |
| `ARCA_TLS_KEY` | | Llave del certificado |
| `--max-object-bytes` | 8 GiB | Tope de un objeto |

Las mismas opciones existen como flags: `--addr`, `--data`, `--region`, `--access-key`, `--secret-key`, `--public-url`, `--tls-cert`, `--tls-key`.

## Dónde quedan los datos

```text
data/
  credentials.json
  b/<bucket>/o/   objetos
  b/<bucket>/m/   metadatos
  b/<bucket>/u/   subidas multipart en curso
```

Borrar el directorio borra los archivos. Cópiarlo copia el cofre entero.

## Desarrollo

```bash
go test ./...
```

Hace falta Go 1.24 o superior. La imagen usa Go 1.27.

## Licencia

MIT. Puedes usarlo, modificarlo y publicarlo. El aviso de copyright tiene que seguir en las copias.
