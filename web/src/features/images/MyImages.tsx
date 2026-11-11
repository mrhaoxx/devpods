// A datalist of the user's saved images for image inputs: free text stays
// possible, the saved images are offered as suggestions.
import { useMe, useSnapshots } from "../../data/queries";

export const MY_IMAGES_LIST = "devpod-my-images";

/** MyImagesList renders the <datalist>; give the input list={MY_IMAGES_LIST}. */
export function MyImagesList() {
  const on = !!useMe().data?.features.snapshots;
  const q = useSnapshots({}, on);
  const images = [...new Set((q.data?.items ?? []).filter((s) => s.phase === "Succeeded").map((s) => s.image))];
  if (!on || images.length === 0) return null;
  return (
    <datalist id={MY_IMAGES_LIST} data-testid="my-images">
      {images.map((i) => (
        <option key={i} value={i} label="我的镜像" />
      ))}
    </datalist>
  );
}
