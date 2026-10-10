/** One file, together with the path it should be stored under. */
export type PickedFile = {
  file: File
  path: string
}

/**
 * Reads a file input's selection.
 *
 * A folder pick reports a relative path per file, which is what keeps
 * ./assets/app.js beside the index.html that links it.
 */
export function pickFromInput(files: FileList): PickedFile[] {
  return Array.from(files, (file) => ({
    file,
    path: file.webkitRelativePath || file.name,
  }))
}

/** Flattens a drag-and-drop payload, walking into any folder that was dropped. */
export async function pickFromDrop(transfer: DataTransfer): Promise<PickedFile[]> {
  const entries = Array.from(transfer.items)
    .filter((item) => item.kind === "file")
    .map((item) => item.webkitGetAsEntry())

  if (entries.some((entry) => entry === null)) {
    return pickFromInput(transfer.files)
  }

  const walked = await Promise.all(entries.map((entry) => readEntry(entry!, "")))
  return walked.flat()
}

async function readEntry(entry: FileSystemEntry, prefix: string): Promise<PickedFile[]> {
  const path = prefix === "" ? entry.name : `${prefix}/${entry.name}`

  if (entry.isFile) {
    const file = await readFile(entry as FileSystemFileEntry)
    return [{ file, path }]
  }
  if (!entry.isDirectory) {
    return []
  }

  const children = await readChildren(entry as FileSystemDirectoryEntry)
  const walked = await Promise.all(children.map((child) => readEntry(child, path)))
  return walked.flat()
}

/** readEntries hands back at most a hundred entries per call, so it must be drained. */
async function readChildren(directory: FileSystemDirectoryEntry): Promise<FileSystemEntry[]> {
  const reader = directory.createReader()
  const children: FileSystemEntry[] = []

  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => {
      reader.readEntries(
        (entries) => resolve(entries),
        (error) => reject(error),
      )
    })
    if (batch.length === 0) {
      return children
    }
    children.push(...batch)
  }
}

function readFile(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => {
    entry.file(resolve, reject)
  })
}

/**
 * Packs picked files into a stored (uncompressed) zip.
 *
 * A browser cannot hand the backend a real zip of a folder, and the backend
 * takes a directory as one archive rather than as many parts, so the bytes have
 * to be assembled here. Everything is stored rather than deflated: there is no
 * compression library available without a dependency, and a browser's
 * CompressionStream only does the other direction.
 */
export async function zipPicked(picked: PickedFile[]): Promise<File> {
  const encoder = new TextEncoder()
  const table = crcTable()

  const body: BlobPart[] = []
  const directory: BlobPart[] = []
  let bodySize = 0
  let directorySize = 0

  for (const { file, path } of picked) {
    const name = encoder.encode(path)
    const data = new Uint8Array(await file.arrayBuffer())
    const crc = crc32(data, table)

    const header = new Uint8Array(30 + name.length)
    const local = new DataView(header.buffer)
    local.setUint32(0, 0x04034b50, true)
    local.setUint16(4, 20, true)
    local.setUint32(14, crc, true)
    local.setUint32(18, data.length, true)
    local.setUint32(22, data.length, true)
    local.setUint16(26, name.length, true)
    header.set(name, 30)

    const entry = new Uint8Array(46 + name.length)
    const central = new DataView(entry.buffer)
    central.setUint32(0, 0x02014b50, true)
    central.setUint16(4, 20, true)
    central.setUint16(6, 20, true)
    central.setUint32(16, crc, true)
    central.setUint32(20, data.length, true)
    central.setUint32(24, data.length, true)
    central.setUint16(28, name.length, true)
    central.setUint32(42, bodySize, true)
    entry.set(name, 46)

    body.push(header, data)
    directory.push(entry)
    bodySize += header.length + data.length
    directorySize += entry.length
  }

  const end = new Uint8Array(22)
  const trailer = new DataView(end.buffer)
  trailer.setUint32(0, 0x06054b50, true)
  trailer.setUint16(8, picked.length, true)
  trailer.setUint16(10, picked.length, true)
  trailer.setUint32(12, directorySize, true)
  trailer.setUint32(16, bodySize, true)

  return new File([...body, ...directory, end], "site.zip", {
    type: "application/zip",
  })
}

/** What was picked, decided from the contents rather than asked about. */
export type Detected = { kind: "zip"; archive: File } | { kind: "files"; files: PickedFile[] }

/**
 * Works out how to send a set of picked files.
 *
 * A lone archive goes up as itself. Anything else goes as separate parts, with
 * a shared wrapper directory removed first, because the server rejects an
 * upload whose root has no index.html.
 *
 * The stripping rule mirrors the server's own detectSingleRoot: a directory is
 * only removed when every file sits inside the same one. Applying it more
 * eagerly would flatten a site that legitimately has folders at its root.
 */
export async function detectUpload(picked: PickedFile[]): Promise<Detected> {
  const only = picked[0]
  if (picked.length === 1 && only !== undefined && (await isZip(only.file))) {
    return { kind: "zip", archive: only.file }
  }
  return { kind: "files", files: stripSharedRoot(picked) }
}

/** Reads the local file header every zip starts with. */
async function isZip(file: File): Promise<boolean> {
  const head = new Uint8Array(await file.slice(0, 4).arrayBuffer())
  return (
    head.length === 4 &&
    head[0] === 0x50 &&
    head[1] === 0x4b &&
    head[2] === 0x03 &&
    head[3] === 0x04
  )
}

function stripSharedRoot(picked: PickedFile[]): PickedFile[] {
  const roots = new Set(picked.map(({ path }) => path.split("/")[0] ?? ""))
  if (roots.size !== 1) return picked

  const [root] = [...roots]
  // A loose file has no directory to strip, so leave the whole set alone.
  if (picked.some(({ path }) => path.split("/").length < 2)) return picked

  return picked.map(({ file, path }) => ({ file, path: path.slice(root!.length + 1) }))
}

function crcTable(): Uint32Array {
  const table = new Uint32Array(256)
  for (let i = 0; i < 256; i += 1) {
    let value = i
    for (let bit = 0; bit < 8; bit += 1) {
      value = value & 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1
    }
    table[i] = value >>> 0
  }
  return table
}

function crc32(data: Uint8Array, table: Uint32Array): number {
  let crc = 0xffffffff
  for (let i = 0; i < data.length; i += 1) {
    crc = table[(crc ^ data[i]!) & 0xff]! ^ (crc >>> 8)
  }
  return (crc ^ 0xffffffff) >>> 0
}
