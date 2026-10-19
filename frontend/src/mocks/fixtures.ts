import type { Media } from '../api/client'

export const folderNames = ['Movies', 'Recordings', 'Family', 'Downloads', 'Animation', 'Travel']
// Every demo video is demo.mp4 at 20.687s, so they share one sheet. Its geometry is
// what spritePlan(20.687) produces on the server, and it is served as a static file
// the same way the poster images are, rather than through a mocked API route.
const demoSprite = { url: '/media/sprite.jpg', cols: 10, rows: 3, count: 21, interval: 1 }
export const items: Media[] = [
  { id: 'holiday', folderId: 'recordings', name: 'My Holiday Recording 2026.mp4', kind: 'video', image: '/media/beach.jpg', src: '/media/demo.mp4', resolution: '1280 × 720', size: '7.3 MB', format: 'MP4', duration: 20.687, progress: 8, sprite: demoSprite },
  { id: 'alpine', folderId: 'trips', name: 'Alpine Views.mp4', kind: 'video', image: '/media/alpine.jpg', src: '/media/demo.mp4', resolution: '1280 × 720', size: '7.3 MB', format: 'MP4', duration: 20.687, progress: 4, sprite: demoSprite },
  { id: 'island', folderId: 'travel', name: 'Phi Phi Island.jpg', kind: 'image', image: '/media/island.jpg', src: '/media/island.jpg', resolution: '1200 × 800', size: '289 KB', format: 'JPG', duration: 0, progress: 0 },
  { id: 'drone', folderId: 'recordings', name: 'Drone Footage.mp4', kind: 'video', image: '/media/forest.jpg', src: '/media/demo.mp4', resolution: '1280 × 720', size: '7.3 MB', format: 'MP4', duration: 20.687, progress: 0, sprite: demoSprite },
  { id: 'forest', folderId: 'family', name: 'A walk in the forest.jpg', kind: 'image', image: '/media/forest.jpg', src: '/media/forest.jpg', resolution: '1200 × 800', size: '415 KB', format: 'JPG', duration: 0, progress: 0 },
  { id: 'coast', folderId: 'movies', name: 'A little escape.mp4', kind: 'video', image: '/media/island.jpg', src: '/media/demo.mp4', resolution: '1280 × 720', size: '7.3 MB', format: 'MP4', duration: 20.687, progress: 0, sprite: demoSprite },
  { id: 'mountains', folderId: '', name: 'Mountain mornings.jpg', kind: 'image', image: '/media/alpine.jpg', src: '/media/alpine.jpg', resolution: '1200 × 800', size: '308 KB', format: 'JPG', duration: 0, progress: 0 },
]
