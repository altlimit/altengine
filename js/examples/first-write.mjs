import { AltEngine } from "@altengine/sdk";

const instance = process.env.ALTENGINE_DATASTORE;
if (!instance) throw new Error("Set ALTENGINE_DATASTORE to your datastore instance name.");

const db = new AltEngine().datastore(instance, { namespace: "sdk-quickstart" });
const key = "first-write";
await db.put("examples", [{ key, data: { message: "Hello from AltEngine" } }]);

const saved = await db.get("examples", key);
if (saved?.data?.message !== "Hello from AltEngine") {
  throw new Error("The datastore read did not return the message just written.");
}
console.log(saved.key, saved.data.message);
