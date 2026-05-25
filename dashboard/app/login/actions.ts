"use server";

import { signIn } from "@/auth";

export async function signInWithEmail(formData: FormData) {
  await signIn("nodemailer", formData);
}
